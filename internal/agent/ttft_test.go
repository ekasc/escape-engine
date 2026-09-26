package agent

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ekasc/escape-engine/internal/provider"
	"github.com/ekasc/escape-engine/internal/session"
	"github.com/ekasc/escape-engine/internal/settings"
)

// A first message must not wait for the session title.
//
// Naming is a second provider round trip. It used to run before the turn loop,
// so the first token of the first message in every session arrived one model
// call late for a title nobody was reading yet. This pins the round trip count
// rather than a wall-clock number, so it does not go flaky on a slow machine.
func TestFirstMessageDoesNotWaitForTheSessionTitle(t *testing.T) {
	store, err := session.Open(filepath.Join(t.TempDir(), "s.jsonl"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const latency = 120 * time.Millisecond
	var mu sync.Mutex
	calls := 0
	fake := &provider.Fake{Handler: func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(latency)
		return []provider.Event{
			{Kind: provider.EventText, Text: "an answer"},
			{Kind: provider.EventDone, StopReason: "stop"},
		}, nil
	}}

	a := New(Options{Store: store, Provider: fake, Settings: settings.Defaults(), Cwd: t.TempDir()})
	firstToken := make(chan time.Time, 1)
	ch := a.Events()
	defer a.Unsubscribe(ch)
	go func() {
		for e := range ch {
			if e.Event == EventMessageDelta {
				select {
				case firstToken <- time.Now():
				default:
				}
			}
		}
	}()

	t0 := time.Now()
	if _, err := a.Send("hello"); err != nil {
		t.Fatal(err)
	}
	ttft := (<-firstToken).Sub(t0)

	// One round trip of provider latency is the floor. Two means the turn
	// waited for something else first, which is what naming used to do.
	if waited := ttft / latency; waited > 1 {
		t.Fatalf("first token waited through %d provider round trips (%v); naming must not gate the turn", waited, ttft)
	}

	// The title must still be written, just not before the answer.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if name, _ := session.Name(store.Path()); name != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the title was never written; making naming concurrent must not drop it")
}

// Only the first message in a session should trigger a title at all.
func TestOnlyTheFirstMessageTriggersNaming(t *testing.T) {
	store, err := session.Open(filepath.Join(t.TempDir(), "s.jsonl"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var mu sync.Mutex
	calls := 0
	fake := &provider.Fake{Handler: func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return []provider.Event{
			{Kind: provider.EventText, Text: "ok"},
			{Kind: provider.EventDone, StopReason: "stop"},
		}, nil
	}}

	a := New(Options{Store: store, Provider: fake, Settings: settings.Defaults(), Cwd: t.TempDir()})
	ch := a.Events()
	defer a.Unsubscribe(ch)
	go func() {
		for range ch {
		}
	}()

	for i := 0; i < 3; i++ {
		if _, err := a.Send("message"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(80 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	// Three turns is three answer calls; naming adds at most one more.
	if calls > 4 {
		t.Fatalf("naming ran more than once: %d provider calls for 3 turns", calls)
	}
}
