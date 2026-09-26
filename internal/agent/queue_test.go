package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ekasc/escape/engine/internal/provider"
)

// recordHandler returns a handler that records the last user text of each
// real turn (skipping the auto-naming meta-call).
func recordHandler(recorded *[]string, mu *sync.Mutex) func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
	return func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		if IsNamingRequest(req) || IsRecapRequest(req) {
			return []provider.Event{{Kind: provider.EventText, Text: "t"}, {Kind: provider.EventDone, StopReason: "stop"}}, nil
		}
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				mu.Lock()
				*recorded = append(*recorded, req.Messages[i].Text)
				mu.Unlock()
				break
			}
		}
		return []provider.Event{{Kind: provider.EventText, Text: "ok"}, {Kind: provider.EventDone, StopReason: "stop"}}, nil
	}
}

func TestFollowUpQueue(t *testing.T) {
	var mu sync.Mutex
	var recorded []string
	ag, _, _ := newTestAgent(t, recordHandler(&recorded, &mu))

	ch := ag.Events()
	defer ag.Unsubscribe(ch)

	if _, err := ag.Send("first"); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.FollowUp("second"); err != nil {
		t.Fatal(err)
	}

	settles := 0
	timeout := time.After(10 * time.Second)
	for settles < 2 {
		select {
		case ev := <-ch:
			if ev.Event == EventSettled {
				settles++
			}
		case <-timeout:
			t.Fatalf("timed out; settles=%d recorded=%v", settles, recorded)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(recorded) != 2 || recorded[0] != "first" || recorded[1] != "second" {
		t.Fatalf("recorded = %v, want [first second]", recorded)
	}
}

func TestRetryDisabled(t *testing.T) {
	ag, _, fake := newTestAgent(t, nil)
	ag.opts.Settings.Retry.Enabled = false
	calls := 0
	fake.Handler = func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		if IsNamingRequest(req) {
			return []provider.Event{{Kind: provider.EventText, Text: "t"}, {Kind: provider.EventDone, StopReason: "stop"}}, nil
		}
		calls++
		return nil, &provider.RetryableError{Status: 503, Msg: "boom"}
	}

	if _, err := ag.Send("x"); err != nil {
		t.Fatal(err)
	}
	waitSettle(t, ag, ReasonError)
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (retry disabled)", calls)
	}
}
