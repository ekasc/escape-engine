package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekasc/escape-engine/internal/provider"
	"github.com/ekasc/escape-engine/internal/session"
	"github.com/ekasc/escape-engine/internal/tools"
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
		Tools:    tools.Default(tools.Deps{Cwd: dir}),
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
		if IsNamingRequest(req) {
			return []provider.Event{
				{Kind: provider.EventText, Text: "Hi there title"},
				{Kind: provider.EventDone, StopReason: "stop"},
			}, nil
		}
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
	if info.Name != "Hi there title" {
		t.Errorf("title = %q", info.Name)
	}
	_ = turnID
}

func TestAutoNamingOnce(t *testing.T) {
	ag, store, fake := newTestAgent(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		if IsNamingRequest(req) {
			return []provider.Event{
				{Kind: provider.EventText, Text: "Fix the bug"},
				{Kind: provider.EventDone, StopReason: "stop"},
			}, nil
		}
		return []provider.Event{
			{Kind: provider.EventText, Text: "ok"},
			{Kind: provider.EventDone, StopReason: "stop"},
		}, nil
	})

	if _, err := ag.Send("please fix the bug"); err != nil {
		t.Fatal(err)
	}
	waitSettle(t, ag, ReasonDone)
	if _, err := ag.Send("and add a test"); err != nil {
		t.Fatal(err)
	}
	waitSettle(t, ag, ReasonDone)

	// Naming fires once, then two turns: 3 provider calls total.
	if fake.Calls != 3 {
		t.Fatalf("calls = %d, want 3 (naming + 2 turns)", fake.Calls)
	}

	entries, err := session.ReadAll(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, e := range entries {
		if e.Type == session.TypeSessionInfo && e.Name != "" {
			titles = append(titles, e.Name)
		}
	}
	if len(titles) != 1 || titles[0] != "Fix the bug" {
		t.Fatalf("titles = %v, want exactly [Fix the bug]", titles)
	}
}

func TestRecapPersistsAndKeepsTitle(t *testing.T) {
	ag, store, _ := newTestAgent(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		if IsNamingRequest(req) {
			return []provider.Event{
				{Kind: provider.EventText, Text: "Fix the bug"},
				{Kind: provider.EventDone, StopReason: "stop"},
			}, nil
		}
		if IsRecapRequest(req) {
			return []provider.Event{
				{Kind: provider.EventText, Text: "Recap: fixed the parser and added a test."},
				{Kind: provider.EventDone, StopReason: "stop"},
			}, nil
		}
		return []provider.Event{
			{Kind: provider.EventText, Text: "ok"},
			{Kind: provider.EventDone, StopReason: "stop"},
		}, nil
	})

	if _, err := ag.Send("please fix the bug"); err != nil {
		t.Fatal(err)
	}
	waitSettle(t, ag, ReasonDone)

	line, err := ag.Recap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "Recap: ") {
		t.Fatalf("recap line = %q", line)
	}

	entries, _ := session.ReadAll(store.Path())
	// Title lookup (last non-empty session_info name) must still resolve.
	if got := session.LastName(entries); got != "Fix the bug" {
		t.Errorf("title after recap = %q, want %q", got, "Fix the bug")
	}
	// The recap is persisted in a session_info entry that carries the title.
	var recapEntry *session.Entry
	for i := range entries {
		if entries[i].Type == session.TypeSessionInfo && entries[i].Recap != "" {
			recapEntry = &entries[i]
		}
	}
	if recapEntry == nil || recapEntry.Recap != line {
		t.Fatalf("recap entry = %+v", recapEntry)
	}
	if recapEntry.Name != "Fix the bug" {
		t.Errorf("recap entry name = %q, want %q", recapEntry.Name, "Fix the bug")
	}
}

func TestRecapWithoutTitle(t *testing.T) {
	ag, store, _ := newTestAgent(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		// Naming fails (best-effort): no title is written.
		if IsNamingRequest(req) {
			return nil, errors.New("naming unavailable")
		}
		if IsRecapRequest(req) {
			return []provider.Event{
				{Kind: provider.EventText, Text: "Recap: no title yet."},
				{Kind: provider.EventDone, StopReason: "stop"},
			}, nil
		}
		return []provider.Event{
			{Kind: provider.EventText, Text: "ok"},
			{Kind: provider.EventDone, StopReason: "stop"},
		}, nil
	})

	if _, err := ag.Send("do work"); err != nil {
		t.Fatal(err)
	}
	waitSettle(t, ag, ReasonDone)

	line, err := ag.Recap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "Recap: ") {
		t.Fatalf("recap line = %q", line)
	}
	entries, _ := session.ReadAll(store.Path())
	if got := session.LastName(entries); got != "" {
		t.Errorf("title = %q, want empty", got)
	}
	// The recap is still persisted (sidecar field on a session_info entry).
	var found bool
	for _, e := range entries {
		if e.Type == session.TypeSessionInfo && e.Recap == line {
			found = true
		}
	}
	if !found {
		t.Error("recap not persisted when no title exists")
	}
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

	if fake.Calls != 3 {
		t.Fatalf("provider calls = %d, want 3 (naming + 2 turns)", fake.Calls)
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

	// The tool result must be persisted with toolCallId (the desktop shell contract).
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

func TestApprovalDeniesSensitiveTool(t *testing.T) {
	ag, store, _ := newTestAgent(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		if IsNamingRequest(req) {
			return []provider.Event{{Kind: provider.EventText, Text: "Approval test"}, {Kind: provider.EventDone, StopReason: "stop"}}, nil
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" {
			return []provider.Event{
				{Kind: provider.EventToolCall, ToolCall: provider.ToolCall{ID: "write_approval", Name: "bash", Args: map[string]any{"command": "touch created-by-agent"}}},
				{Kind: provider.EventDone, StopReason: "tool_calls"},
			}, nil
		}
		return []provider.Event{{Kind: provider.EventDone, StopReason: "stop"}}, nil
	})
	ag.opts.ApprovalMode = ApprovalAsk

	events := ag.Events()
	defer ag.Unsubscribe(events)
	if _, err := ag.Send("create a file"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	approved := false
	settled := false
	for !settled {
		select {
		case ev := <-events:
			switch ev.Event {
			case EventApprovalRequested:
				if ev.ToolCallID != "write_approval" || ev.Name != "bash" {
					t.Fatalf("approval event = %+v", ev)
				}
				if err := ag.AnswerApproval(ev.ToolCallID, false); err != nil {
					t.Fatal(err)
				}
				approved = true
			case EventSettled:
				settled = true
			}
		case <-deadline:
			t.Fatal("approval turn did not settle")
		}
	}
	if !approved {
		t.Fatal("approval was not requested")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(store.Path()), "created-by-agent")); !os.IsNotExist(err) {
		t.Fatalf("sensitive tool ran despite denial: %v", err)
	}
	entries, _ := session.ReadAll(store.Path())
	found := false
	for _, entry := range entries {
		if entry.Message != nil && entry.Message.ToolCallID == "write_approval" && entry.Message.Text(false) == "tool execution denied by user" {
			found = true
		}
	}
	if !found {
		t.Fatal("denial was not persisted as a tool result")
	}
}

func TestRenameSession(t *testing.T) {
	ag, store, _ := newTestAgent(t, nil)
	if err := ag.Rename("Daily driver"); err != nil {
		t.Fatal(err)
	}
	name, err := session.Name(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if name != "Daily driver" {
		t.Fatalf("session name = %q", name)
	}
}

func TestSetApprovalMode(t *testing.T) {
	ag, _, _ := newTestAgent(t, nil)
	if err := ag.SetApprovalMode(ApprovalAsk); err != nil {
		t.Fatal(err)
	}
	if got := ag.State().ApprovalMode; got != ApprovalAsk {
		t.Fatalf("approval mode = %q", got)
	}
	if err := ag.SetApprovalMode("sometimes"); err == nil {
		t.Fatal("invalid approval mode was accepted")
	}
}

func TestSwitchStore(t *testing.T) {
	ag, _, _ := newTestAgent(t, nil)
	dir := t.TempDir()
	next, err := session.Open(filepath.Join(dir, "next.jsonl"), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if err := ag.SwitchStore(next); err != nil {
		t.Fatal(err)
	}
	if got := ag.opts.Store.Path(); got != next.Path() {
		t.Fatalf("active store = %q, want %q", got, next.Path())
	}
}

func TestQuestionPausesAndResumes(t *testing.T) {
	ag, store, _ := newTestAgent(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		if IsNamingRequest(req) {
			return []provider.Event{{Kind: provider.EventText, Text: "Question test"}, {Kind: provider.EventDone, StopReason: "stop"}}, nil
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" {
			return []provider.Event{
				{Kind: provider.EventToolCall, ToolCall: provider.ToolCall{ID: "q1", Name: "question", Args: map[string]any{"question": "Which environment?", "choices": []string{"dev", "prod"}}}},
				{Kind: provider.EventDone, StopReason: "tool_calls"},
			}, nil
		}
		if last.Role == "tool" {
			return []provider.Event{{Kind: provider.EventText, Text: "Using " + last.Text}, {Kind: provider.EventDone, StopReason: "stop"}}, nil
		}
		return nil, errors.New("unexpected call")
	})

	events := ag.Events()
	defer ag.Unsubscribe(events)
	if _, err := ag.Send("deploy it"); err != nil {
		t.Fatal(err)
	}
	answer := make(chan struct{})
	go func() {
		for ev := range events {
			if ev.Event == EventQuestionRequested {
				if ev.Question != "Which environment?" || ev.ToolCallID != "q1" {
					t.Errorf("question event = %+v", ev)
				}
				if err := ag.AnswerQuestion("q1", "dev"); err != nil {
					t.Errorf("answer question: %v", err)
				}
				close(answer)
				return
			}
		}
	}()
	select {
	case <-answer:
	case <-time.After(2 * time.Second):
		t.Fatal("question was not requested")
	}
	waitSettle(t, ag, ReasonDone)
	entries, _ := session.ReadAll(store.Path())
	found := false
	for _, e := range entries {
		if e.Message != nil && e.Message.ToolCallID == "q1" && e.Message.Text(false) == "dev" {
			found = true
		}
	}
	if !found {
		t.Fatal("question answer was not persisted")
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
		if IsNamingRequest(req) {
			return []provider.Event{
				{Kind: provider.EventText, Text: "retry title"},
				{Kind: provider.EventDone, StopReason: "stop"},
			}, nil
		}
		if fake.Calls <= 2 {
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
	if fake.Calls != 3 { // naming + one failed attempt + one retry
		t.Fatalf("calls = %d, want 3", fake.Calls)
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
	if fake.Calls != 2 { // naming (aborted) + the redirected turn
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
	if !strings.HasPrefix(got, "agent_start,turn_started,") || !strings.Contains(got, "message_start,") || !strings.Contains(got, "message_delta,") || !strings.Contains(got, "message_end,") || !strings.HasSuffix(got, ",agent_settled") {
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
	if len(seen) != 3 { // naming + two turns
		t.Fatalf("seen = %d", len(seen))
	}
	// The first turn call must include the first user+assistant pair; the
	// second turn call must retain it too.
	if !strings.Contains(seen[1], "user:first turn") {
		t.Errorf("first turn history lost: %s", seen[1])
	}
	if !strings.Contains(seen[2], "user:first turn") {
		t.Errorf("second turn history lost: %s", seen[2])
	}
}

func TestStripThinking(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Recap: <think>I will summarize now</think> fixed the bug.", "Recap:  fixed the bug."}, // cleanRecap collapses the gap
		{"<think>only thinking</think>", ""},
		{"<antml:thinking>t</antml:thinking>Title", "Title"},
		{"no tags here", "no tags here"},
	}
	for _, c := range cases {
		if got := stripThinking(c.in); got != c.want {
			t.Errorf("stripThinking(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCleanRecapStripsThinking(t *testing.T) {
	got := cleanRecap("<think>The user wants a summary.</think> Recap: rewrote the provider layer.")
	if got != "Recap: rewrote the provider layer." {
		t.Fatalf("cleanRecap = %q", got)
	}
}
