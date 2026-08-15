package provider

import (
	"context"
	"io"
	"sync"
)

// Fake is a scriptable provider for tests and offline demos. The Handler is
// invoked at Stream() time with the request; it returns the complete event
// sequence (or an error, e.g. a *RetryableError to exercise retry logic).
type Fake struct {
	mu      sync.Mutex
	Handler func(ctx context.Context, req Request) ([]Event, error)
	Calls   int
}

// FakeStream replays a pre-computed event list.
type FakeStream struct {
	events []Event
	idx    int
}

func (f *Fake) Stream(ctx context.Context, req Request) (Stream, error) {
	f.mu.Lock()
	f.Calls++
	handler := f.Handler
	f.mu.Unlock()

	if handler == nil {
		return &FakeStream{events: []Event{{Kind: EventDone, StopReason: "stop"}}}, nil
	}
	events, err := handler(ctx, req)
	if err != nil {
		return nil, err
	}
	return &FakeStream{events: events}, nil
}

func (s *FakeStream) Next() (Event, error) {
	if s.idx >= len(s.events) {
		return Event{}, io.EOF
	}
	ev := s.events[s.idx]
	s.idx++
	return ev, nil
}

func (s *FakeStream) Close() error { return nil }
