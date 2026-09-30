// Package provider defines the model-facing interface: a streaming chat
// provider with tool-call support. v1 ships one HTTP implementation
// (OpenAI-compatible /chat/completions) and a scriptable fake for tests and
// offline demos.
package provider

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// ToolCall is a model-requested function invocation.
type ToolCall struct {
	ID   string
	Name string
	Args map[string]any
}

// Image is one image attachment in a message. Both provider wires serialize it:
// OpenAI-compatible providers send an image_url content part and the ChatGPT
// provider an input_image block. Whether it is attached at all is decided before
// the wire is built, from the model's declared input modality.
type Image struct {
	MimeType string // e.g. "image/png"
	Data     []byte // raw image bytes
}

// DataURI renders the image as a base64 data URI for the wire:
// "data:<mime>;base64,<payload>".
func (im *Image) DataURI() string {
	return "data:" + im.MimeType + ";base64," + base64.StdEncoding.EncodeToString(im.Data)
}

// ParseDataURI decodes a base64 data URI ("data:<mime>;base64,<payload>") into
// an Image, e.g. from a read tool result.
func ParseDataURI(uri string) (*Image, error) {
	rest, ok := strings.CutPrefix(uri, "data:")
	if !ok {
		return nil, fmt.Errorf("not a data URI")
	}
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok {
		return nil, fmt.Errorf("malformed data URI")
	}
	mime, _, _ := strings.Cut(meta, ";")
	if mime == "" {
		return nil, fmt.Errorf("data URI missing MIME type")
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("data URI: %w", err)
	}
	return &Image{MimeType: mime, Data: data}, nil
}

// Message is one chat message in provider (wire) terms.
type Message struct {
	Role         string // "system" | "user" | "assistant" | "tool"
	Text         string
	Thinking     string // assistant reasoning (not sent back on later turns)
	ToolCallID   string // for role "tool"
	ToolCallName string // for role "tool"
	ToolCalls    []ToolCall
	// Image, when set, attaches an image to this message (typically role
	// "user" or "tool", e.g. the result of reading an image file).
	Image  *Image
	Images []*Image
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
	Input              int
	Output             int
	CacheRead          int
	CacheWrite         int
	TotalTokens        int
	CacheReadAvailable bool
}

// CacheHitRate returns cached prompt tokens divided by total prompt tokens.
// The boolean is false when the provider did not report cache accounting.
func CacheHitRate(usage Usage) (float64, bool) {
	if !usage.CacheReadAvailable || usage.Input <= 0 {
		return 0, false
	}
	return float64(usage.CacheRead) / float64(usage.Input), true
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
