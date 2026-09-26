package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenAI is a streaming client for OpenAI-compatible /chat/completions
// endpoints (OpenAI, DeepSeek, Ollama, vLLM, ...).
type OpenAI struct {
	BaseURL string // e.g. https://api.openai.com/v1
	APIKey  string
	Model   string
	// ReasoningEffort, when non-empty, is sent as `reasoning_effort`
	// (OpenAI-compatible reasoning level: minimal/low/medium/high/xhigh).
	ReasoningEffort string
	HTTP            *http.Client

	// Logf, when set, receives non-prompt diagnostic lines (never prompt or
	// tool payloads — mirroring pi's rule about not mirroring prompts).
	Logf func(format string, args ...any)
}

// NewOpenAI builds a client from env-style configuration.
func NewOpenAI(baseURL, apiKey, model string) *OpenAI {
	return &OpenAI{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{}, // streaming: no overall timeout; ctx governs
	}
}

func (c *OpenAI) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// SetReasoningEffort updates the reasoning effort for subsequent calls.
func (c *OpenAI) SetReasoningEffort(level string) { c.ReasoningEffort = level }

// Stream implements Provider.
func (c *OpenAI) Stream(ctx context.Context, req Request) (Stream, error) {
	model := c.Model
	if model == "" {
		model = req.Model
	}

	effort := c.ReasoningEffort
	for {
		body := map[string]any{
			"model":          model,
			"messages":       wireMessages(req.Messages),
			"stream":         true,
			"stream_options": map[string]any{"include_usage": true},
		}
		if req.MaxTokens > 0 {
			body["max_tokens"] = req.MaxTokens
		}
		if len(req.Tools) > 0 {
			body["tools"] = wireTools(req.Tools)
		}
		if effort != "" && effort != "off" {
			body["reasoning_effort"] = effort
		}

		payload, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}

		url := c.BaseURL + "/chat/completions"
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if c.APIKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
		}

		c.logf("provider: POST %s model=%s messages=%d tools=%d effort=%q", url, model, len(req.Messages), len(req.Tools), effort)
		resp, err := c.HTTP.Do(httpReq)
		if err != nil {
			return nil, &RetryableError{Msg: fmt.Sprintf("transport error: %v", err)}
		}
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			status := resp.StatusCode
			// Model rejects the reasoning level → fall back to the lowest
			// supported effort instead of failing the turn.
			if isReasoningRejection(status, msg) && effort != "" {
				next := nextLowerEffort(effort)
				c.logf("provider: model rejected reasoning_effort=%q (%s), retrying with %q", effort, strings.TrimSpace(string(msg)), next)
				effort = next
				continue
			}
			if status == http.StatusTooManyRequests || status >= 500 {
				return nil, &RetryableError{Status: status, Msg: fmt.Sprintf("provider status %d: %s", status, strings.TrimSpace(string(msg)))}
			}
			return nil, fmt.Errorf("provider status %d: %s", status, strings.TrimSpace(string(msg)))
		}

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
		return &sseStream{sc: sc, resp: resp, logf: c.logf}, nil
	}
}

// --- wire encoding ---

func wireMessages(msgs []Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			w := map[string]any{"role": "assistant", "content": m.Text}
			if len(m.ToolCalls) > 0 {
				calls := make([]map[string]any, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					args := "{}"
					if tc.Args != nil {
						if b, err := json.Marshal(tc.Args); err == nil {
							args = string(b)
						}
					}
					calls = append(calls, map[string]any{
						"id":   tc.ID,
						"type": "function",
						"function": map[string]any{
							"name":      tc.Name,
							"arguments": args,
						},
					})
				}
				w["tool_calls"] = calls
			}
			out = append(out, w)
		case "tool":
			out = append(out, map[string]any{"role": "tool", "tool_call_id": m.ToolCallID, "content": wireContent(m)})
		default: // "user", "system"
			out = append(out, map[string]any{"role": m.Role, "content": wireContent(m)})
		}
	}
	return out
}

// wireContent renders a message's content as a plain string, or — when the
// message carries an image — as an array of content parts:
// [{"type":"text","text":...}, {"type":"image_url","image_url":{"url":"data:<mime>;base64,..."}}]
// (OpenAI multimodal format).
func wireContent(m Message) any {
	images := m.Images
	if m.Image != nil {
		images = append([]*Image{m.Image}, images...)
	}
	if len(images) == 0 {
		return m.Text
	}
	parts := make([]map[string]any, 0, len(images)+1)
	if m.Text != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Text})
	}
	for _, image := range images {
		if image != nil {
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": image.DataURI()},
			})
		}
	}
	return parts
}

func wireTools(tools []ToolSpec) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		})
	}
	return out
}

// --- SSE stream ---

// sseStream parses an OpenAI-style SSE body into events. Tool calls arrive as
// index-addressed fragments across chunks and are reassembled; they are
// emitted in index order once the finish reason (or [DONE]/EOF) arrives.
type sseStream struct {
	sc   *bufio.Scanner
	resp *http.Response
	logf func(format string, args ...any)

	usage        Usage
	toolCalls    map[int]ToolCall
	argsRaw      map[int]string
	order        []int
	pending      []ToolCall
	finished     bool
	finishReason string
	done         bool
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func (s *sseStream) Next() (Event, error) {
	for {
		// Emit reassembled tool calls (in order) before anything else.
		if len(s.pending) > 0 {
			tc := s.pending[0]
			s.pending = s.pending[1:]
			return Event{Kind: EventToolCall, ToolCall: tc}, nil
		}
		if s.done {
			return Event{}, io.EOF
		}
		if s.finished {
			s.done = true
			return Event{Kind: EventDone, StopReason: s.finishReason, Usage: s.usage}, nil
		}

		if !s.sc.Scan() {
			if err := s.sc.Err(); err != nil {
				s.done = true
				return Event{}, err
			}
			s.finish("stop")
			continue
		}
		line := bytes.TrimSpace(s.sc.Bytes())
		if len(line) == 0 || line[0] == ':' || !bytes.HasPrefix(line, []byte("data:")) {
			continue // keepalives, comments, ignore
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(data, []byte("[DONE]")) {
			s.finish("stop")
			continue
		}
		ev := s.parseChunk(data)
		if ev.Kind != EventNone {
			return ev, nil
		}
	}
}

// finish marks the stream complete: queue any un-emitted tool calls (with
// fully reassembled arguments) and record the final stop reason.
func (s *sseStream) finish(reason string) {
	if s.finished {
		return
	}
	s.finished = true
	s.finishReason = reason
	for _, idx := range s.order {
		tc, ok := s.toolCalls[idx]
		if !ok {
			continue
		}
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_%d", idx)
		}
		if tc.Args == nil {
			tc.Args = map[string]any{}
		}
		// Argument fragments arrive as partial JSON; concatenate the raw
		// strings and parse once at the end.
		if raw, ok := s.argsRaw[idx]; ok && raw != "" {
			if err := json.Unmarshal([]byte(raw), &tc.Args); err != nil {
				s.logf("provider: tool call args not valid JSON: %v", err)
			}
		}
		s.pending = append(s.pending, tc)
	}
	s.order = nil
	s.toolCalls = nil
	s.argsRaw = nil
}

// parseChunk converts one SSE data payload into an event (EventNone if there
// is nothing to surface).
func (s *sseStream) parseChunk(data []byte) Event {
	var ch streamChunk
	if err := json.Unmarshal(data, &ch); err != nil {
		s.logf("provider: skipping bad stream chunk: %v", err)
		return NoneEvent
	}

	if ch.Usage != nil {
		s.usage = Usage{
			Input:       ch.Usage.PromptTokens,
			Output:      ch.Usage.CompletionTokens,
			TotalTokens: ch.Usage.TotalTokens,
		}
		if ch.Usage.PromptTokensDetails != nil {
			s.usage.CacheRead = ch.Usage.PromptTokensDetails.CachedTokens
			s.usage.CacheReadAvailable = true
		}
	}

	var ev Event
	if len(ch.Choices) > 0 {
		delta := ch.Choices[0].Delta
		switch {
		case delta.ReasoningContent != "":
			ev = Event{Kind: EventThinking, Thinking: delta.ReasoningContent}
		case delta.Content != "":
			ev = Event{Kind: EventText, Text: delta.Content}
		}
		for _, tc := range delta.ToolCalls {
			if s.toolCalls == nil {
				s.toolCalls = map[int]ToolCall{}
				s.argsRaw = map[int]string{}
			}
			cur := s.toolCalls[tc.Index]
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Function.Name != "" {
				cur.Name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				s.argsRaw[tc.Index] += tc.Function.Arguments
			}
			s.toolCalls[tc.Index] = cur
			s.appendIndex(tc.Index)
			ev = NoneEvent // tool fragments never double as text
		}
		if fr := ch.Choices[0].FinishReason; fr != nil && *fr != "" {
			s.finish(*fr)
		}
	}
	return ev
}

func (s *sseStream) appendIndex(idx int) {
	for _, i := range s.order {
		if i == idx {
			return
		}
	}
	s.order = append(s.order, idx)
}

func (s *sseStream) Close() error {
	// Drain remaining body in the background so the connection can be reused;
	// never block the agent loop on a close.
	go func() {
		for s.sc.Scan() {
		}
	}()
	return s.resp.Body.Close()
}
