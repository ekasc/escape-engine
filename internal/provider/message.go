// Package provider defines the model-facing interface: a streaming chat
// provider with tool-call support. v1 ships one HTTP implementation
// (OpenAI-compatible /chat/completions) and a scriptable fake for tests and
// offline demos.
package provider

import (
	"context"
	"strconv"
)

// ToolCall is a model-requested function invocation.
type ToolCall struct {
	ID   string
	Name string
	Args map[string]any
}

// Message is one chat message in provider (wire) terms.
type Message struct {
	Role         string // "system" | "user" | "assistant" | "tool"
	Text         string
	Thinking     string // assistant reasoning (not sent back on later turns)
	ToolCallID   string // for role "tool"
	ToolCallName string // for role "tool"
	ToolCalls    []ToolCall
}

// ToolSpec declares one tool to the model.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON schema object
}

// Request is a complete model call.
type Request struct {
	Model     string
	Messages  []Message
	Tools     []ToolSpec
	MaxTokens int
}

// Usage mirrors pi's token accounting.
type Usage struct {
	Input       int
	Output      int
	CacheRead   int
	CacheWrite  int
	TotalTokens int
}

// EventKind discriminates stream events.
type EventKind int

const (
	EventText EventKind = iota
	EventThinking
	EventToolCall
	EventDone
	EventNone // internal: no event to surface
)

// Event is one element of a model stream.
type Event struct {
	Kind       EventKind
	Text       string
	Thinking   string
	ToolCall   ToolCall
	StopReason string // on EventDone: "stop" | "length" | "tool_calls" | ...
	Usage      Usage  // on EventDone, when the provider reports it
}

// NoneEvent is the sentinel for "no event to surface" (Kind == EventNone).
var NoneEvent = Event{Kind: EventNone}

// Stream yields events until EventDone or io.EOF.
type Stream interface {
	Next() (Event, error)
	Close() error
}

// Provider streams model responses. Implementations must be safe for
// concurrent use.
type Provider interface {
	Stream(ctx context.Context, req Request) (Stream, error)
}

// RetryableError marks provider failures that are safe to retry (429/5xx,
// transient transport errors).
type RetryableError struct {
	Status int
	Msg    string
}

func (e *RetryableError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return "retryable provider error (status " + strconv.Itoa(e.Status) + ")"
}
