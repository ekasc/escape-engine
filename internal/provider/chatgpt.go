package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ChatGPT is a Provider backed by the ChatGPT-subscription Codex Responses
// endpoint (chatgpt.com/backend-api/codex/responses), authenticated with
// OpenAI OAuth tokens from a device-flow login (see DeviceLogin). It speaks
// the Responses wire format, not /chat/completions.
type ChatGPT struct {
	ClientID string
	Tokens   *TokenSet
	Model    string
	// ReasoningEffort, when non-empty, is sent as responses-format
	// reasoning.effort (low/medium/high/...).
	ReasoningEffort string
	HTTP            *http.Client

	// Logf, when set, receives non-prompt diagnostic lines.
	Logf func(format string, args ...any)
}

// NewChatGPT builds a subscription-backed provider from stored OAuth tokens.
func NewChatGPT(tokens *TokenSet, model string) *ChatGPT {
	return &ChatGPT{
		ClientID: ChatGPTClientID,
		Tokens:   tokens,
		Model:    model,
		HTTP:     &http.Client{},
	}
}

func (c *ChatGPT) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// SetReasoningEffort updates the reasoning effort for subsequent calls.
func (c *ChatGPT) SetReasoningEffort(level string) { c.ReasoningEffort = level }

// Stream implements Provider: one call to the Codex Responses endpoint,
// translating the SSE event stream into provider events. A 401 triggers a
// token refresh and a single retry.
func (c *ChatGPT) Stream(ctx context.Context, req Request) (Stream, error) {
	model := c.Model
	if model == "" {
		model = req.Model
	}

	effort := c.ReasoningEffort
	refreshed := false
	for {
		body := map[string]any{
			"model":  model,
			"input":  wireInput(req.Messages),
			"stream": true,
		}
		if req.MaxTokens > 0 {
			body["max_output_tokens"] = req.MaxTokens
		}
		if len(req.Tools) > 0 {
			body["tools"] = wireResponsesTools(req.Tools)
		}
		if effort != "" && effort != "off" {
			body["reasoning"] = map[string]any{"effort": effort}
		}
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}

		resp, err := c.post(ctx, payload)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && !refreshed {
			resp.Body.Close()
			c.logf("chatgpt: 401, refreshing OAuth token and retrying")
			if err := c.Tokens.Refresh(ctx, c.ClientID); err != nil {
				return nil, fmt.Errorf("chatgpt auth: %w", err)
			}
			refreshed = true
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			resp.Body.Close()
			return nil, &RetryableError{Status: resp.StatusCode, Msg: fmt.Sprintf("chatgpt: HTTP %d", resp.StatusCode)}
		}
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			// Model rejects the reasoning level → fall back down the ladder.
			if isReasoningRejection(resp.StatusCode, msg) && effort != "" {
				effort = nextLowerEffort(effort)
				c.logf("chatgpt: rejected reasoning effort, retrying with %q", effort)
				continue
			}
			return nil, fmt.Errorf("chatgpt: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
		}
		return &chatgptStream{sc: bufio.NewScanner(resp.Body), resp: resp, logf: c.logf}, nil
	}
}

// post sends the payload with the current access token.
func (c *ChatGPT) post(ctx context.Context, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ChatGPTChatURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Tokens.AccessToken)
	return c.HTTP.Do(req)
}

// wireInput translates chat/completions messages into the Responses `input`
// array (message / function_call / function_call_output items).
func wireInput(msgs []Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			out = append(out, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  m.Text,
			})
		case "assistant":
			w := map[string]any{
				"type":    "message",
				"role":    "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": m.Text}},
			}
			if len(m.ToolCalls) > 0 {
				calls := make([]map[string]any, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					args, _ := json.Marshal(tc.Args)
					if tc.ID == "" {
						tc.ID = fmt.Sprintf("call_%d", len(calls))
					}
					calls = append(calls, map[string]any{
						"type":      "function_call",
						"id":        tc.ID,
						"call_id":   tc.ID,
						"name":      tc.Name,
						"arguments": string(args),
					})
				}
				w["tool_calls"] = calls
			}
			out = append(out, w)
		default: // "user", "system"
			out = append(out, map[string]any{
				"type":    "message",
				"role":    m.Role,
				"content": []any{map[string]any{"type": "input_text", "text": m.Text}},
			})
		}
	}
	return out
}

// wireResponsesTools translates ToolSpecs into Responses `tools` items.
func wireResponsesTools(tools []ToolSpec) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"type":        "function",
			"name":        t.Name,
			"description": t.Description,
			"parameters":  t.Parameters,
		})
	}
	return out
}

// --- SSE stream ------------------------------------------------------------

// chatgptStream parses the Responses-endpoint SSE stream into provider events.
// Tool calls arrive as function_call_arguments.done events with the full
// arguments JSON, so no index reassembly is needed.
type chatgptStream struct {
	sc      *bufio.Scanner
	resp    *http.Response
	logf    func(format string, args ...any)
	usage   Usage
	done    bool
	pending []Event
}

// responseEvent is one `event:`/`data:` pair from the Responses SSE stream.
type responseEvent struct {
	Type  string          `json:"type"`
	Delta json.RawMessage `json:"delta"`
	Item  json.RawMessage `json:"item"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Response *struct {
		Status string `json:"status"`
		Usage  *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
			InputDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	} `json:"response"`
}

func (s *chatgptStream) Next() (Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.done {
			return Event{}, io.EOF
		}
		if !s.sc.Scan() {
			s.done = true
			if err := s.sc.Err(); err != nil {
				return Event{}, err
			}
			return Event{Kind: EventDone, StopReason: "stop", Usage: s.usage}, nil
		}
		line := bytes.TrimSpace(s.sc.Bytes())
		if len(line) == 0 || line[0] == ':' || !bytes.HasPrefix(line, []byte("data:")) {
			continue // keepalives, comments
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 {
			continue
		}
		var ev responseEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			s.logf("chatgpt: skipping bad stream event: %v", err)
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			var d struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(ev.Delta, &d) == nil && d.Text != "" {
				return Event{Kind: EventText, Text: d.Text}, nil
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			var d struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(ev.Delta, &d) == nil && d.Text != "" {
				return Event{Kind: EventThinking, Thinking: d.Text}, nil
			}
		case "response.function_call_arguments.done":
			var d struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			if json.Unmarshal(ev.Delta, &d) == nil && d.Name != "" {
				tc := ToolCall{ID: d.ID, Name: d.Name}
				if strings.TrimSpace(d.Arguments) != "" {
					_ = json.Unmarshal([]byte(d.Arguments), &tc.Args)
				}
				return Event{Kind: EventToolCall, ToolCall: tc}, nil
			}
		case "response.completed":
			s.done = true
			if ev.Response != nil && ev.Response.Usage != nil {
				u := ev.Response.Usage
				s.usage = Usage{
					Input:       u.InputTokens,
					Output:      u.OutputTokens,
					TotalTokens: u.TotalTokens,
				}
				if u.InputDetails != nil {
					s.usage.CacheRead = u.InputDetails.CachedTokens
					s.usage.CacheReadAvailable = true
				}
			}
			return Event{Kind: EventDone, StopReason: "stop", Usage: s.usage}, nil
		case "response.failed":
			s.done = true
			if ev.Error != nil && ev.Error.Message != "" {
				return Event{}, errors.New(ev.Error.Message)
			}
			return Event{}, errors.New("chatgpt: response failed")
		}
	}
}

// Close releases the underlying response body.
func (s *chatgptStream) Close() error { return s.resp.Body.Close() }
