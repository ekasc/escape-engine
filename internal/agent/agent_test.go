package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekasc/pi-go/internal/provider"
	"github.com/ekasc/pi-go/internal/session"
	"github.com/ekasc/pi-go/internal/tools"
)

func newTestAgent(t *testing.T, handler func(ctx context.Context, req provider.Request) ([]provider.Event, error)) (*Agent, *session.Store, *provider.Fake) {
	t.Helper()
	dir := t.TempDir()
	store, err := session.Open(filepath.Join(dir, "test.jsonl"), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	fake := &provider.Fake{Handler: handler}
	ag := New(Options{
		Store:    store,
		Provider: fake,
		Tools:    tools.Default(dir),
		Cwd:      dir,
		Model:    "fake-model",
	})
	return ag, store, fake
}

func waitSettle(t *testing.T, ag *Agent, want string) {
	t.Helper()
	// Fast path: the turn already settled before we could subscribe (the bus
	// does not replay). State() remembers the last settle reason.
	if st := ag.State(); st.State == StateIdle && st.SettleReason != "" {
		if st.SettleReason != want {
			t.Fatalf("settle reason = %q, want %q", st.SettleReason, want)
		}
		return
	}
	done, unsub := startSettleWatcher(ag)
	defer unsub()
	select {
	case got := <-done:
		if got != want {
			t.Fatalf("settle reason = %q, want %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("agent did not settle")
	}
}

// startSettleWatcher subscribes to the settle event BEFORE the turn starts
// (the bus does not replay past events), returning a channel for the reason.
func startSettleWatcher(ag *Agent) (<-chan string, func()) {
	done := make(chan string, 1)
	ch := ag.Events()
	go func() {
		for ev := range ch {
			if ev.Event == EventSettled {
				done <- ev.Reason
				return
			}
		}
	}()
	return done, func() { ag.Unsubscribe(ch) }
}

func TestTurnSimple(t *testing.T) {
	ag, store, _ := newTestAgent(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		return []provider.Event{
			{Kind: provider.EventText, Text: "Hello!"},
			{Kind: provider.EventDone, StopReason: "stop"},
		}, nil
	})

	turnID, err := ag.Send("hi there")
	if err != nil {
		t.Fatal(err)
	}
	waitSettle(t, ag, ReasonDone)

	if ag.State().State != StateIdle {
		t.Errorf("state = %v", ag.State().State)
	}
	entries, _ := session.ReadAll(store.Path())
	var roles []string
	for _, e := range entries {
		if e.Message != nil {
			roles = append(roles, e.Message.Role)
		}
	}
	want := []string{"user", "assistant"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("roles = %v, want %v", roles, want)
	}

	// Session file must carry an assistant message with text + a title.
	info, err := session.ReadInfo(store.Path())
	if err != nil || info == nil {
		t.Fatalf("info: %v", err)
	}
	if !strings.Contains(info.Name, "hi there") {
		t.Errorf("title = %q", info.Name)
	}
	_ = turnID
}

func TestTurnToolUse(t *testing.T) {
	ag, store, fake := newTestAgent(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" && last.Text == "list files" {
			return []provider.Event{
				{Kind: provider.EventText, Text: "Checking…"},
				{Kind: provider.EventToolCall, ToolCall: provider.ToolCall{ID: "call_1", Name: "bash", Args: map[string]any{"command": "echo tool-ran"}}},
				{Kind: provider.EventDone, StopReason: "tool_calls"},
			}, nil
		}
		// Second round: sees the tool result.
		if last.Role == "tool" {
			return []provider.Event{
				{Kind: provider.EventText, Text: "Result: " + last.Text},
				{Kind: provider.EventDone, StopReason: "stop"},
			}, nil
		}
		return nil, errors.New("unexpected call")
	})

	_, err := ag.Send("list files")
	if err != nil {
		t.Fatal(err)
	}
	waitSettle(t, ag, ReasonDone)

	if fake.Calls != 2 {
		t.Fatalf("provider calls = %d, want 2", fake.Calls)
	}

	entries, _ := session.ReadAll(store.Path())
	var roles []string
	for _, e := range entries {
		if e.Message != nil {
			roles = append(roles, e.Message.Role)
		}
	}
	want := "user,assistant,toolResult,assistant"
	if strings.Join(roles, ",") != want {
		t.Fatalf("roles = %v, want %v", roles, want)
	}

	// The tool result must be persisted with toolCallId (Babylon contract).
	var tr *session.Message
	for _, e := range entries {
		if e.Message != nil && e.Message.Role == session.RoleToolResult {
			tr = e.Message
		}
	}
	if tr == nil {
		t.Fatal("no toolResult entry")
	}
	if tr.ToolCallID != "call_1" || tr.ToolName != "bash" {
		t.Fatalf("toolResult = %+v", tr)
	}
	if !strings.Contains(tr.Text(false), "tool-ran") {
		t.Errorf("tool result text = %q", tr.Text(false))
	}
	// Assistant stopReason must be toolUse for the tool-call turn.
	var asst *session.Message
	for _, e := range entries {
		if e.Message != nil && e.Message.Role == session.RoleAssistant && e.Message.StopReason == session.StopToolUse {
			asst = e.Message
		}
	}
	if asst == nil {
		t.Errorf("no toolUse assistant entry in %+v", entries)
	}
}

func TestStopMidTurn(t *testing.T) {
	ag, store, _ := newTestAgent(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		<-ctx.Done() // block until stopped
		return nil, ctx.Err()
	})

	settled, unsub := startSettleWatcher(ag)
	defer unsub()

	if _, err := ag.Send("long task"); err != nil {
		t.Fatal(err)
	}
	ag.Stop()
	select {
	case got := <-settled:
		if got != ReasonStopped {
			t.Fatalf("settle reason = %q, want stopped", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("agent did not settle")
	}

	// Stopped before any output: user message persisted, no assistant entry.
	entries, _ := session.ReadAll(store.Path())
	roles := []string{}
	for _, e := range entries {
		if e.Message != nil {
			roles = append(roles, e.Message.Role)
		}
	}
	if strings.Join(roles, ",") != "user" {
		t.Fatalf("roles = %v", roles)
	}
}

func TestStopAfterPartialText(t *testing.T) {
	ag, store, _ := newTestAgent(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		return []provider.Event{
			{Kind: provider.EventText, Text: "partial"},
			{Kind: provider.EventDone, StopReason: "stop"}, // arrives before Stop lands
		}, nil
	})
	_ = store
	_, err := ag.Send("x")
	if err != nil {
		t.Fatal(err)
	}
	waitSettle(t, ag, ReasonDone)
}

func TestProviderErrorRetry(t *testing.T) {
	ag, _, fake := newTestAgent(t, nil)
	fake.Handler = func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		if fake.Calls <= 1 {
			return nil, &provider.RetryableError{Status: 503, Msg: "boom"}
		}
		return []provider.Event{
			{Kind: provider.EventText, Text: "recovered"},
			{Kind: provider.EventDone, StopReason: "stop"},
		}, nil
	}

	if _, err := ag.Send("retry me"); err != nil {
		t.Fatal(err)
	}
	waitSettle(t, ag, ReasonDone)
	if fake.Calls != 2 {
		t.Fatalf("calls = %d, want 2 (one retry)", fake.Calls)
	}
}

func TestProviderFatalError(t *testing.T) {
	ag, _, _ := newTestAgent(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		return nil, errors.New("auth failed")
	})
	_, err := ag.Send("x")
	if err != nil {
		t.Fatal(err)
	}
	waitSettle(t, ag, ReasonError)
	if ag.State().SettleReason != ReasonError {
		t.Errorf("settleReason = %q", ag.State().SettleReason)
	}
}

func TestEmptyToolResult(t *testing.T) {
	ag, _, _ := newTestAgent(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" {
			return []provider.Event{
				{Kind: provider.EventToolCall, ToolCall: provider.ToolCall{ID: "c1", Name: "read", Args: map[string]any{"path": "missing.txt"}}},
				{Kind: provider.EventDone, StopReason: "tool_calls"},
			}, nil
		}
		return []provider.Event{
			{Kind: provider.EventText, Text: "done"},
			{Kind: provider.EventDone, StopReason: "stop"},
		}, nil
	})
	if _, err := ag.Send("read missing"); err != nil {
		t.Fatal(err)
	}
	waitSettle(t, ag, ReasonDone)
}

func TestUnknownTool(t *testing.T) {
	ag, _, _ := newTestAgent(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" {
			return []provider.Event{
				{Kind: provider.EventToolCall, ToolCall: provider.ToolCall{ID: "c1", Name: "teleport", Args: map[string]any{}}},
				{Kind: provider.EventDone, StopReason: "tool_calls"},
			}, nil
		}
		return []provider.Event{
			{Kind: provider.EventText, Text: "noted"},
			{Kind: provider.EventDone, StopReason: "stop"},
		}, nil
	})
	_, err := ag.Send("use teleport")
	if err != nil {
		t.Fatal(err)
	}
	waitSettle(t, ag, ReasonDone)
	// The error surfaced through the tool result, not a crash.
}

func TestSendBusy(t *testing.T) {
	blocker := make(chan struct{})
	ag, _, _ := newTestAgent(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		<-blocker
		return []provider.Event{{Kind: provider.EventDone, StopReason: "stop"}}, nil
	})
	if _, err := ag.Send("one"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := ag.Send("two"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second send err = %v, want ErrBusy", err)
	}
	close(blocker)
	waitSettle(t, ag, ReasonDone)
}

func TestSteer(t *testing.T) {
	var mu sync.Mutex
	block := true
	ag, _, fake := newTestAgent(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		mu.Lock()
		shouldBlock := block
		mu.Unlock()
		if shouldBlock {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		last := req.Messages[len(req.Messages)-1]
		return []provider.Event{
			{Kind: provider.EventText, Text: "answer: " + last.Text},
			{Kind: provider.EventDone, StopReason: "stop"},
		}, nil
	})

	_, err := ag.Send("first")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	block = false
	mu.Unlock()

	turnID, err := ag.Steer("redirected")
	if err != nil {
		t.Fatal(err)
	}
	waitSettle(t, ag, ReasonDone)
	_ = turnID
	if fake.Calls != 2 {
		t.Fatalf("calls = %d, want 2", fake.Calls)
	}
}

func TestMaxIterations(t *testing.T) {
	ag, _, _ := newTestAgent(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		return []provider.Event{
			{Kind: provider.EventToolCall, ToolCall: provider.ToolCall{ID: "c1", Name: "bash", Args: map[string]any{"command": "true"}}},
			{Kind: provider.EventDone, StopReason: "tool_calls"},
		}, nil
	})
	ag.opts.MaxIterations = 3
	_, err := ag.Send("loop")
	if err != nil {
		t.Fatal(err)
	}
	waitSettle(t, ag, ReasonError)
}

func TestEventStreamOrder(t *testing.T) {
	ag, _, _ := newTestAgent(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		return []provider.Event{
			{Kind: provider.EventText, Text: "a"},
			{Kind: provider.EventText, Text: "b"},
			{Kind: provider.EventDone, StopReason: "stop"},
		}, nil
	})
	ch := ag.Events()
	defer ag.Unsubscribe(ch)

	var order []string
	done := make(chan struct{})
	go func() {
		for ev := range ch {
			order = append(order, ev.Event)
			if ev.Event == EventSettled {
				close(done)
				return
			}
		}
	}()

	if _, err := ag.Send("x"); err != nil {
		t.Fatal(err)
	}
	<-done

	got := strings.Join(order, ",")
	if !strings.HasPrefix(got, "turn_started,") || !strings.Contains(got, "message_start,") || !strings.Contains(got, "message_delta,") || !strings.Contains(got, "message_end,") || !strings.HasSuffix(got, ",agent_settled") {
		t.Fatalf("event order = %s", got)
	}
	if !strings.Contains(got, "agent_settled") {
		t.Fatalf("missing settle: %s", got)
	}
}

func TestTurnKeepsHistory(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	ag, _, _ := newTestAgent(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		mu.Lock()
		var texts []string
		for _, m := range req.Messages {
			texts = append(texts, m.Role+":"+m.Text)
		}
		seen = append(seen, strings.Join(texts, "|"))
		mu.Unlock()
		return []provider.Event{{Kind: provider.EventDone, StopReason: "stop"}}, nil
	})

	ag.Send("first turn")
	waitSettle(t, ag, ReasonDone)
	ag.Send("second turn")
	waitSettle(t, ag, ReasonDone)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("seen = %d", len(seen))
	}
	// The second call must include the first user+assistant pair.
	if !strings.Contains(seen[1], "user:first turn") {
		t.Errorf("history lost across turns: %s", seen[1])
	}
}
