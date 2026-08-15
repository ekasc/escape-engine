// Package agent implements the agent loop: user prompt -> model call
// (streaming) -> tool calls -> observe -> loop until settled, with
// stop/steer control and an event bus for the CLI and the stdio RPC layer.
package agent

import "sync"

// Event is one entry on the agent's event bus, serialized to the wire by the
// rpc layer as {"type":"event","event":<name>,...}.
type Event struct {
	Type        string         `json:"type"`
	Event       string         `json:"event"`
	TurnID      string         `json:"turnId,omitempty"`
	SessionID   string         `json:"sessionId,omitempty"`
	SessionFile string         `json:"sessionFile,omitempty"`
	Cwd         string         `json:"cwd,omitempty"`
	Role        string         `json:"role,omitempty"`
	Text        string         `json:"text,omitempty"`
	MessageID   string         `json:"messageId,omitempty"`
	Model       string         `json:"model,omitempty"`
	Timestamp   int64          `json:"timestamp,omitempty"`
	Iteration   int            `json:"iteration,omitempty"`
	ToolCallID  string         `json:"toolCallId,omitempty"`
	Name        string         `json:"name,omitempty"`
	Args        map[string]any `json:"args,omitempty"`
	Output      string         `json:"output,omitempty"`
	Error       bool           `json:"error,omitempty"`
	Reason      string         `json:"reason,omitempty"`
	Message     string         `json:"message,omitempty"`
}

// Event names on the wire.
const (
	EventTurnStarted    = "turn_started"
	EventMessageStart   = "message_start"
	EventMessageDelta   = "message_delta"
	EventMessageEnd     = "message_end"
	EventToolCall       = "tool_call"
	EventToolResult     = "tool_result"
	EventSettled        = "agent_settled"
	EventError          = "error"
	EventSessionStarted = "session_started"
)

// Settle reasons for agent_settled.
const (
	ReasonDone    = "done"
	ReasonStopped = "stopped"
	ReasonError   = "error"
)

// Bus is a non-blocking fan-out event bus. Slow subscribers have events
// dropped rather than blocking the agent loop (optimistic-everything).
type Bus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

const busBuffer = 256

// NewBus creates an empty bus.
func NewBus() *Bus {
	return &Bus{subs: map[chan Event]struct{}{}}
}

// Subscribe registers a channel and returns it. Unsubscribe when done.
func (b *Bus) Subscribe() chan Event {
	ch := make(chan Event, busBuffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes a channel.
func (b *Bus) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

// Publish fans an event out to all subscribers without blocking.
func (b *Bus) Publish(ev Event) {
	ev.Type = "event"
	b.mu.Lock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default: // drop for slow subscribers
		}
	}
	b.mu.Unlock()
}
