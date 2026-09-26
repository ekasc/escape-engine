package provider

import (
	"context"
	"net/http"
	"strings"
)

// OpenCodeZen is the OpenAI-compatible client for OpenCode Zen.
//
// Zen exposes free and paid models through the same chat-completions shape as
// the other OpenAI-compatible providers. An empty API key is valid for free
// models, so the client only adds an Authorization header when a key exists.
type OpenCodeZen struct {
	BaseURL         string
	APIKey          string
	Model           string
	ReasoningEffort string
	HTTP            *http.Client
	Logf            func(format string, args ...any)
}

// NewOpenCodeZen builds a client for the OpenCode Zen provider.
func NewOpenCodeZen(apiKey, model string) *OpenCodeZen {
	if model == "" {
		model = OpenCodeZenDefaultModel
	}
	return &OpenCodeZen{
		BaseURL: OpenCodeZenBaseURL,
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{},
	}
}

// SetReasoningEffort updates the reasoning level for subsequent calls.
func (c *OpenCodeZen) SetReasoningEffort(level string) { c.ReasoningEffort = level }

// Stream implements Provider.
func (c *OpenCodeZen) Stream(ctx context.Context, req Request) (Stream, error) {
	model := c.Model
	if model == "" {
		model = req.Model
	}
	model = strings.TrimPrefix(model, "opencode-zen/")
	model = strings.TrimPrefix(model, "opencode/")
	client := NewOpenAI(c.BaseURL, c.APIKey, model)
	client.ReasoningEffort = c.ReasoningEffort
	client.Logf = c.Logf
	if c.HTTP != nil {
		client.HTTP = c.HTTP
	}
	return client.Stream(ctx, req)
}
