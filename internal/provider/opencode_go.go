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

type OpenCodeGo struct {
	BaseURL         string
	APIKey          string
	Model           string
	SessionID       string
	ReasoningEffort string
	HTTP            *http.Client
	Logf            func(format string, args ...any)
}

func NewOpenCodeGo(apiKey, model string) *OpenCodeGo {
	return &OpenCodeGo{BaseURL: OpenCodeGoBaseURL, APIKey: apiKey, Model: model, HTTP: &http.Client{}}
}

func (c *OpenCodeGo) SetReasoningEffort(level string) { c.ReasoningEffort = level }

func (c *OpenCodeGo) SetSessionID(id string) { c.SessionID = id }

func (c *OpenCodeGo) Stream(ctx context.Context, req Request) (Stream, error) {
	model := c.Model
	if model == "" {
		model = req.Model
	}
	if strings.HasPrefix(model, "opencode-go/") {
		model = strings.TrimPrefix(model, "opencode-go/")
	}
	if openCodeGoUsesResponses(model) {
		return c.streamResponses(ctx, req, model)
	}
	client := *c.HTTP
	client.Transport = openCodeSessionTransport{base: c.HTTP.Transport, sessionID: c.SessionID}
	return (&OpenAI{BaseURL: c.BaseURL, APIKey: c.APIKey, Model: model, ReasoningEffort: c.ReasoningEffort, HTTP: &client, Logf: c.Logf}).Stream(ctx, req)
}

type openCodeSessionTransport struct {
	base      http.RoundTripper
	sessionID string
}

func (t openCodeSessionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.sessionID != "" {
		req = req.Clone(req.Context())
		req.Header.Set("x-opencode-session", t.sessionID)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func openCodeGoUsesResponses(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "grok-")
}

func (c *OpenCodeGo) streamResponses(ctx context.Context, req Request, model string) (Stream, error) {
	body := map[string]any{"model": model, "input": wireInput(req.Messages), "stream": true}
	if req.MaxTokens > 0 {
		body["max_output_tokens"] = req.MaxTokens
	}
	if len(req.Tools) > 0 {
		body["tools"] = wireResponsesTools(req.Tools)
	}
	if c.ReasoningEffort != "" && c.ReasoningEffort != "off" {
		body["reasoning"] = map[string]any{"effort": c.ReasoningEffort}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/responses", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	if c.SessionID != "" {
		httpReq.Header.Set("x-opencode-session", c.SessionID)
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, &RetryableError{Msg: fmt.Sprintf("opencode-go transport error: %v", err)}
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return nil, &RetryableError{Status: resp.StatusCode, Msg: fmt.Sprintf("opencode-go: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))}
		}
		return nil, fmt.Errorf("opencode-go: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	return &chatgptStream{sc: sc, resp: resp, logf: c.Logf}, nil
}
