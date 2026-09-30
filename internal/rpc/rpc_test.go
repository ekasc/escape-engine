package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekasc/escape-engine/internal/agent"
	"github.com/ekasc/escape-engine/internal/provider"
	"github.com/ekasc/escape-engine/internal/session"
	"github.com/ekasc/escape-engine/internal/tools"
)

// newTestServer wires a fake-provider agent behind an rpc.Server over pipes.
func newTestServer(t *testing.T, handler func(ctx context.Context, req provider.Request) ([]provider.Event, error)) (*Server, *session.Store, io.WriteCloser, *bufio.Reader, <-chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	store, err := session.Open(filepath.Join(dir, "s.jsonl"), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	ag := agent.New(agent.Options{
		Store:    store,
		Provider: &provider.Fake{Handler: handler},
		Tools:    tools.Default(tools.Deps{Cwd: dir}),
		Cwd:      dir,
		Model:    "fake-model",
	})

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := NewServer(ag, store, session.DefaultRoot(), dir, outW)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		srv.Serve(ctx, inR)
	}()
	go srv.Relay(ctx, ag.Events())

	t.Cleanup(func() {
		inW.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
	return srv, store, inW, bufio.NewReader(outR), done
}

func writeReq(t *testing.T, w io.Writer, id int, method string, params any) {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	if _, err := w.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

// waitStarted consumes the synchronous session_started event. Serve writes it
// before reading stdin, so every test must drain it before its first request
// (a pipe write blocks until the reader catches up).
func waitStarted(t *testing.T, r *bufio.Reader) {
	t.Helper()
	readUntil(t, r, func(o map[string]any) bool {
		return o["type"] == "event" && o["event"] == agent.EventSessionStarted
	})
}

// readUntil scans stdout lines until the predicate matches; returns all lines.
// The read is deadline-bounded: a blocking read can still hang until the test
// timeout, so the per-read deadline is enforced with a read-timeout wrapper.
func readUntil(t *testing.T, r *bufio.Reader, pred func(map[string]any) bool) []map[string]any {
	t.Helper()
	var lines []map[string]any
	deadline := time.After(10 * time.Second)
	for {
		type result struct {
			line string
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			line, err := r.ReadString('\n')
			ch <- result{line, err}
		}()
		select {
		case <-deadline:
			t.Fatalf("timed out; saw %d lines: %v", len(lines), lines)
		case res := <-ch:
			if res.err != nil {
				if res.err == io.EOF && len(lines) > 0 {
					return lines
				}
				t.Fatalf("read: %v", res.err)
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(res.line), &obj); err != nil {
				t.Fatalf("bad line %q: %v", res.line, err)
			}
			lines = append(lines, obj)
			if pred(obj) {
				return lines
			}
		}
	}
}

func TestServeRoundTrip(t *testing.T) {
	_, store, inW, outR, _ := newTestServer(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" {
			return []provider.Event{
				{Kind: provider.EventText, Text: "hi " + last.Text},
				{Kind: provider.EventToolCall, ToolCall: provider.ToolCall{ID: "c1", Name: "bash", Args: map[string]any{"command": "echo done"}}},
				{Kind: provider.EventDone, StopReason: "tool_calls"},
			}, nil
		}
		return []provider.Event{
			{Kind: provider.EventText, Text: "finished"},
			{Kind: provider.EventDone, StopReason: "stop"},
		}, nil
	})

	waitStarted(t, outR)

	writeReq(t, inW, 1, "send", map[string]any{"text": "world"})
	lines := readUntil(t, outR, func(o map[string]any) bool {
		return o["type"] == "event" && o["event"] == agent.EventSettled
	})

	var sawToolCall, sawToolResult, sawResponse bool
	var settleReason string
	for _, o := range lines {
		switch {
		case o["id"] != nil:
			if o["jsonrpc"] == "2.0" && float64(1) == toF(o["id"]) {
				sawResponse = true
				if o["error"] != nil {
					t.Fatalf("send response error: %v", o["error"])
				}
			}
		case o["type"] == "event":
			switch o["event"] {
			case agent.EventToolCall:
				sawToolCall = true
				if o["name"] != "bash" {
					t.Errorf("tool_call name = %v", o["name"])
				}
			case agent.EventToolResult:
				sawToolResult = true
			case agent.EventSettled:
				settleReason, _ = o["reason"].(string)
			}
		}
	}
	if !sawResponse || !sawToolCall || !sawToolResult {
		t.Fatalf("missing events: response=%v toolCall=%v toolResult=%v", sawResponse, sawToolCall, sawToolResult)
	}
	if settleReason != "done" {
		t.Errorf("settle reason = %q", settleReason)
	}

	// The session file must now contain the full transcript.
	entries, _ := session.ReadAll(store.Path())
	if len(entries) < 4 { // header + user + assistant + toolResult + assistant
		t.Fatalf("entries = %d", len(entries))
	}
	var roles []string
	for _, e := range entries {
		if e.Message != nil {
			roles = append(roles, e.Message.Role)
		}
	}
	want := "user,assistant,toolResult,assistant"
	if strings.Join(roles, ",") != want {
		t.Fatalf("roles = %v", roles)
	}

	// The file must be readable by a the desktop shell-style tail reader.
	tail, start, err := session.Tail(store.Path(), 2048)
	if err != nil || len(tail) == 0 || start <= 0 {
		t.Fatalf("tail: %v start=%d n=%d", err, start, len(tail))
	}
}

func TestRPCStateAndStop(t *testing.T) {
	block := make(chan struct{})
	_, _, inW, outR, _ := newTestServer(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []provider.Event{{Kind: provider.EventDone, StopReason: "stop"}}, nil
	})

	waitStarted(t, outR)
	writeReq(t, inW, 2, "state", nil)
	lines := readUntil(t, outR, func(o map[string]any) bool { return o["id"] != nil })
	state := lines[len(lines)-1]["result"].(map[string]any)
	if state["state"] != "idle" {
		t.Errorf("state = %v", state)
	}

	writeReq(t, inW, 3, "send", map[string]any{"text": "long"})
	lines = readUntil(t, outR, func(o map[string]any) bool {
		return o["type"] == "event" && o["event"] == agent.EventTurnStarted
	})
	writeReq(t, inW, 4, "stop", nil)
	lines = readUntil(t, outR, func(o map[string]any) bool {
		return o["type"] == "event" && o["event"] == agent.EventSettled
	})
	if reason := lines[len(lines)-1]["reason"]; reason != "stopped" {
		t.Errorf("settle reason = %v", reason)
	}
	close(block)
}

func TestRPCErrors(t *testing.T) {
	_, _, inW, outR, _ := newTestServer(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		return nil, nil
	})
	waitStarted(t, outR)
	writeReq(t, inW, 5, "bogus", nil)
	lines := readUntil(t, outR, func(o map[string]any) bool { return o["id"] != nil })
	resp := lines[len(lines)-1]
	if resp["error"] == nil {
		t.Fatalf("expected error, got %v", resp)
	}
	if code := resp["error"].(map[string]any)["code"]; code != float64(codeNotFound) {
		t.Errorf("code = %v", code)
	}
}

func TestRPCBusyRejectsSecondSend(t *testing.T) {
	block := make(chan struct{})
	_, _, inW, outR, _ := newTestServer(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []provider.Event{{Kind: provider.EventDone, StopReason: "stop"}}, nil
	})
	waitStarted(t, outR)
	writeReq(t, inW, 6, "send", map[string]any{"text": "one"})
	readUntil(t, outR, func(o map[string]any) bool {
		return o["type"] == "event" && o["event"] == agent.EventTurnStarted
	})
	writeReq(t, inW, 7, "send", map[string]any{"text": "two"})
	lines := readUntil(t, outR, func(o map[string]any) bool { return o["id"] != nil && toF(o["id"]) == 7 })
	resp := lines[len(lines)-1]
	if resp["error"] == nil {
		t.Fatalf("expected busy error, got %v", resp)
	}
	close(block)
}

func TestRPCListSessions(t *testing.T) {
	root := t.TempDir()
	// session.List expects root/<slug>/<file>.jsonl, so nest the files.
	// Each session gets an entry, because opening a store no longer creates its
	// file: a session exists once something has been sent to it.
	for _, name := range []string{"a", "b"} {
		path := filepath.Join(root, session.SlugForDir("/proj"), name+".jsonl")
		s, err := session.Open(path, "/proj")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Append(session.Entry{Type: session.TypeMessage, Message: &session.Message{Role: session.RoleUser, Content: []session.Block{{Type: "text", Text: "hi"}}}}); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}

	ag := agent.New(agent.Options{Store: nil, Provider: &provider.Fake{}, Cwd: root, Model: "m"})
	// Reuse a server pointed at the temp sessions root.
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srvStore, err := session.Open(filepath.Join(root, "server.jsonl"), "/proj")
	if err != nil {
		t.Fatal(err)
	}
	defer srvStore.Close()
	srv := NewServer(ag, srvStore, root, "/proj", outW)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Serve(ctx, inR)
	}()
	t.Cleanup(func() { cancel(); inW.Close(); <-done })

	waitStarted(t, bufio.NewReader(outR))
	writeReq(t, inW, 8, "list-sessions", nil)
	lines := readUntil(t, bufio.NewReader(outR), func(o map[string]any) bool { return o["id"] != nil && toF(o["id"]) == 8 })
	result := lines[len(lines)-1]["result"].(map[string]any)
	sessions, ok := result["sessions"].([]any)
	if !ok {
		t.Fatalf("sessions = %#v (result %#v)", result["sessions"], result)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d", len(sessions))
	}
}

func TestRPCRecap(t *testing.T) {
	_, store, inW, outR, _ := newTestServer(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		if agent.IsNamingRequest(req) {
			return []provider.Event{
				{Kind: provider.EventText, Text: "Session title"},
				{Kind: provider.EventDone, StopReason: "stop"},
			}, nil
		}
		if agent.IsRecapRequest(req) {
			return []provider.Event{
				{Kind: provider.EventText, Text: "Recap: did the work."},
				{Kind: provider.EventDone, StopReason: "stop"},
			}, nil
		}
		return []provider.Event{
			{Kind: provider.EventText, Text: "done"},
			{Kind: provider.EventDone, StopReason: "stop"},
		}, nil
	})

	waitStarted(t, outR)
	// Run a turn first so the session has a title.
	writeReq(t, inW, 1, "send", map[string]any{"text": "do the work"})
	readUntil(t, outR, func(o map[string]any) bool {
		return o["type"] == "event" && o["event"] == agent.EventSettled
	})

	writeReq(t, inW, 2, "recap", nil)
	lines := readUntil(t, outR, func(o map[string]any) bool { return o["id"] != nil && toF(o["id"]) == 2 })
	resp := lines[len(lines)-1]
	if resp["error"] != nil {
		t.Fatalf("recap error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	text, _ := result["text"].(string)
	if !strings.HasPrefix(text, "Recap: ") {
		t.Fatalf("recap text = %q", text)
	}

	// Persisted as a session_info entry with the title carried forward.
	entries, _ := session.ReadAll(store.Path())
	if got := session.LastName(entries); got != "Session title" {
		t.Errorf("title after recap = %q", got)
	}
	var recapFound bool
	for _, e := range entries {
		if e.Type == session.TypeSessionInfo && e.Recap == text {
			recapFound = true
		}
	}
	if !recapFound {
		t.Error("recap not persisted")
	}
}

func TestRPCStateCapabilities(t *testing.T) {
	_, _, inW, outR, _ := newTestServer(t, func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		return []provider.Event{{Kind: provider.EventDone, StopReason: "stop"}}, nil
	})
	waitStarted(t, outR)
	writeReq(t, inW, 9, "state", nil)
	lines := readUntil(t, outR, func(o map[string]any) bool { return o["id"] != nil })
	result := lines[len(lines)-1]["result"].(map[string]any)
	caps, ok := result["capabilities"].([]any)
	if !ok {
		t.Fatalf("capabilities missing: %v", result)
	}
	got := map[string]bool{}
	for _, c := range caps {
		got[c.(string)] = true
	}
	if !got["recap"] || !got["auto-naming"] {
		t.Errorf("capabilities = %v", caps)
	}
}

func toF(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return -1
}
