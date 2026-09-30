package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekasc/escape-engine/internal/agent"
	"github.com/ekasc/escape-engine/internal/provider"
	"github.com/ekasc/escape-engine/internal/session"
	"github.com/ekasc/escape-engine/internal/settings"
	"github.com/ekasc/escape-engine/internal/tools"
)

// fakePiHandler is the canned provider used by the pi protocol tests: every
// call (naming, main turn, compaction summary) returns the fixed "hi" text
// and a done event, so tests exercise the full wire path deterministically.
func fakePiHandler(ctx context.Context, req provider.Request) ([]provider.Event, error) {
	return []provider.Event{
		{Kind: provider.EventText, Text: "hi"},
		{Kind: provider.EventDone, StopReason: "stop"},
	}, nil
}

// piTestServer wires a PiServer over pipes. stdin stays open until the test
// finishes (closed by t.Cleanup), so Serve's stop-on-stdin-EOF never races an
// in-flight turn: prompt responses and agent_settled events are always
// delivered before the test closes the input.
type piTestServer struct {
	t   *testing.T
	srv *PiServer
	// cwd is the project the server booted in, so a test can switch away and
	// back without hard-coding a path.
	cwd         string
	sessionPath string
	inW         *io.PipeWriter
	outR        *bufio.Reader
	done        chan struct{}
	cancel      context.CancelFunc
}

func newPiTestServer(t *testing.T, handler func(ctx context.Context, req provider.Request) ([]provider.Event, error)) *piTestServer {
	t.Helper()
	if handler == nil {
		handler = fakePiHandler
	}
	dir := t.TempDir()
	// The project store, the settings file and the skills all live in the global
	// dir. Without this, a test that adds a project writes the user's real
	// ~/.escape/projects.json and the entry turns up in their sidebar later.
	t.Setenv("ESCAPE_GLOBAL_DIR", filepath.Join(dir, "global"))
	root := filepath.Join(dir, "sessions")
	sessionPath, err := session.NewPath(root, dir)
	if err != nil {
		t.Fatalf("session path: %v", err)
	}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	ctrl := &session.Control{}
	srv, err := NewPiServer(
		&provider.Fake{Handler: handler},
		tools.Default(tools.Deps{Cwd: dir, SessionRoot: root, Control: ctrl}),
		settings.Defaults(),
		dir,
		root,
		sessionPath,
		ctrl,
		outW,
		func(cwd, sessionRoot string, set *settings.Settings) ([]tools.Tool, error) {
			return tools.Default(tools.Deps{Cwd: cwd, SessionRoot: sessionRoot, Control: ctrl}), nil
		},
	)
	if err != nil {
		t.Fatalf("NewPiServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		_ = srv.Serve(ctx, inR)
	}()

	ts := &piTestServer{
		t: t, srv: srv, cwd: dir, sessionPath: sessionPath,
		inW: inW, outR: bufio.NewReader(outR), done: done, cancel: cancel,
	}
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("PiServer.Serve did not return after stdin close")
		}
		cancel()
	})
	return ts
}

// send writes one pi-format request line: {"type": "<command>", ...params}.
func (ts *piTestServer) send(req map[string]any) {
	ts.t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		ts.t.Fatalf("marshal request %v: %v", req, err)
	}
	if _, err := ts.inW.Write(append(b, '\n')); err != nil {
		ts.t.Fatalf("write request: %v", err)
	}
}

// readUntil scans stdout lines until pred matches, reusing the deadline-
// bounded reader from rpc_test.go. Returns every line seen.
func (ts *piTestServer) readUntil(pred func(map[string]any) bool) []map[string]any {
	ts.t.Helper()
	return readUntil(ts.t, ts.outR, pred)
}

// findResponse returns the last response line for the given command.
func findResponse(lines []map[string]any, command string) map[string]any {
	var out map[string]any
	for _, l := range lines {
		if l["type"] == "response" && l["command"] == command {
			out = l
		}
	}
	return out
}

// mustResponse reads until (and including) the response for command and
// returns it, failing the test if it never arrives.
func (ts *piTestServer) mustResponse(command string) map[string]any {
	ts.t.Helper()
	lines := ts.readUntil(func(o map[string]any) bool {
		return o["type"] == "response" && o["command"] == command
	})
	resp := findResponse(lines, command)
	if resp == nil {
		ts.t.Fatalf("no response for %q; lines: %v", command, lines)
	}
	return resp
}

// runPrompt sends a prompt and waits for the turn to settle (agent_settled).
func (ts *piTestServer) runPrompt(message string) {
	ts.t.Helper()
	ts.send(map[string]any{"type": "prompt", "message": message})
	ts.readUntil(func(o map[string]any) bool { return o["type"] == "agent_settled" })
}

func TestPiPrompt(t *testing.T) {
	ts := newPiTestServer(t, nil)

	ts.send(map[string]any{"type": "prompt", "message": "hello"})
	lines := ts.readUntil(func(o map[string]any) bool { return o["type"] == "agent_settled" })

	seen := map[string]bool{}
	var sawDelta bool
	var resp map[string]any
	for _, l := range lines {
		typ, _ := l["type"].(string)
		seen[typ] = true
		if typ == "message_update" {
			if ev, ok := l["assistantMessageEvent"].(map[string]any); ok && ev["type"] == "text_delta" {
				sawDelta = true
			}
		}
		if typ == "response" && l["command"] == "prompt" {
			resp = l
		}
	}
	// The response is written by the dispatch goroutine; it normally lands
	// before agent_settled, but read a little more if it raced past it.
	if resp == nil {
		resp = ts.mustResponse("prompt")
	}

	for _, want := range []string{"agent_start", "turn_start", "message_end", "agent_end", "agent_settled"} {
		if !seen[want] {
			t.Errorf("missing event %q during prompt; saw %v", want, seen)
		}
	}
	if !sawDelta {
		t.Error("no message_update with assistantMessageEvent.type == \"text_delta\"")
	}
	if resp["success"] != true {
		t.Errorf("prompt response success = %v (error=%v)", resp["success"], resp["error"])
	}
}

// The shell cannot find turn boundaries without them, and it treats a stopped
// turn as a different outcome from a failed one. Both facts ride on the wire,
// so both are asserted here rather than assumed to survive the emit mapping.
func TestPiTurnLifecycleCarriesTurnIdentity(t *testing.T) {
	ts := newPiTestServer(t, nil)

	ts.send(map[string]any{"type": "prompt", "message": "hello"})
	lines := ts.readUntil(func(o map[string]any) bool { return o["type"] == "agent_settled" })

	var startID, endID string
	var endState, endReason string
	for _, l := range lines {
		switch l["type"] {
		case "turn_start":
			startID, _ = l["turnId"].(string)
		case "turn_end":
			endID, _ = l["turnId"].(string)
			endState, _ = l["state"].(string)
			endReason, _ = l["reason"].(string)
		}
	}
	if startID == "" {
		t.Error("turn_start carried no turnId; the shell has to invent turn boundaries without it")
	}
	if endID != startID {
		t.Errorf("turn_end turnId = %q, want the turn_start id %q", endID, startID)
	}
	if endState != "completed" {
		t.Errorf("turn_end state = %q, want %q", endState, "completed")
	}
	if endReason == "" {
		t.Error("turn_end carried no reason")
	}
}

// turnEndState is the mapping the shell's three outcomes depend on.
func TestTurnEndState(t *testing.T) {
	for _, tc := range []struct{ reason, want string }{
		{agent.ReasonDone, "completed"},
		{agent.ReasonStopped, "interrupted"},
		{agent.ReasonError, "error"},
		{"something-new", "completed"},
	} {
		if got := turnEndState(tc.reason); got != tc.want {
			t.Errorf("turnEndState(%q) = %q, want %q", tc.reason, got, tc.want)
		}
	}
}

func TestPiGetState(t *testing.T) {
	ts := newPiTestServer(t, nil)

	ts.send(map[string]any{"type": "get_state"})
	resp := ts.mustResponse("get_state")

	if resp["success"] != true {
		t.Fatalf("get_state success = %v (error=%v)", resp["success"], resp["error"])
	}
	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("get_state data = %#v", resp["data"])
	}
	want, _ := filepath.Abs(ts.sessionPath)
	if data["sessionFile"] != want {
		t.Errorf("sessionFile = %v, want %v", data["sessionFile"], want)
	}
	if id, _ := data["sessionId"].(string); id == "" {
		t.Errorf("sessionId empty: %#v", data)
	}
}

func TestPiAvailableThinkingLevels(t *testing.T) {
	ts := newPiTestServer(t, nil)

	ts.send(map[string]any{"type": "get_available_thinking_levels"})
	resp := ts.mustResponse("get_available_thinking_levels")

	if resp["success"] != true {
		t.Fatalf("get_available_thinking_levels success = %v (error=%v)", resp["success"], resp["error"])
	}
	levels, ok := resp["data"].(map[string]any)["levels"].([]any)
	if !ok {
		t.Fatalf("levels = %#v", resp["data"])
	}
	got := map[string]bool{}
	for _, l := range levels {
		got[l.(string)] = true
	}
	if !got["off"] || !got["high"] {
		t.Errorf("levels missing off/high: %v", levels)
	}
}

func TestPiSetModel(t *testing.T) {
	ts := newPiTestServer(t, nil)

	ts.send(map[string]any{"type": "set_model", "model": "deepseek-v4-flash"})
	if resp := ts.mustResponse("set_model"); resp["success"] != true {
		t.Fatalf("set_model success = %v (error=%v)", resp["success"], resp["error"])
	}

	ts.send(map[string]any{"type": "get_state"})
	resp := ts.mustResponse("get_state")
	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("get_state data = %#v", resp["data"])
	}
	if data["model"] != "deepseek-v4-flash" {
		t.Errorf("model = %v, want deepseek-v4-flash", data["model"])
	}
}

func TestPiSessionLifecycle(t *testing.T) {
	ts := newPiTestServer(t, nil)

	ts.send(map[string]any{"type": "list_sessions"})
	list := ts.mustResponse("list_sessions")
	if list["success"] != true {
		t.Fatalf("list_sessions = %v", list)
	}
	// A server that has just started has nothing to show. It used to report one
	// session here, because opening a store created its file: the shell listed
	// that as an "Untitled session" that nobody had ever sent anything to.
	initial, _ := list["data"].(map[string]any)["sessions"].([]any)
	if len(initial) != 0 {
		t.Fatalf("a fresh server should have no sessions, got %v", list["data"])
	}

	ts.send(map[string]any{"type": "new_session"})
	created := ts.mustResponse("new_session")
	if created["success"] != true {
		t.Fatalf("new_session = %v", created)
	}
	createdData := created["data"].(map[string]any)
	newPath := createdData["path"].(string)

	ts.send(map[string]any{"type": "set_session_name", "name": "Named session"})
	if response := ts.mustResponse("set_session_name"); response["success"] != true {
		t.Fatalf("set_session_name = %v", response)
	}

	ts.send(map[string]any{"type": "switch_session", "sessionPath": newPath})
	switched := ts.mustResponse("switch_session")
	if switched["success"] != true {
		t.Fatalf("switch_session = %v", switched)
	}
	ts.send(map[string]any{"type": "get_state"})
	state := ts.mustResponse("get_state")
	stateData := state["data"].(map[string]any)
	if stateData["sessionFile"] != newPath {
		t.Fatalf("active session = %v, want %s", stateData["sessionFile"], newPath)
	}
	ts.runPrompt("after switch")

	// Sending is what makes a session exist. Before this, the file was created
	// by being opened, so the list filled with sessions that had no messages.
	ts.send(map[string]any{"type": "list_sessions"})
	after := ts.mustResponse("list_sessions")
	entries, _ := after["data"].(map[string]any)["sessions"].([]any)
	if len(entries) == 0 {
		t.Fatal("a session with a prompt in it should be listed")
	}
}

// Switching project rebinds the agent to a new directory, and it used to mint a
// new session file every time. Navigating between projects therefore filled the
// session list with empty sessions, one per switch.
func TestPiSwitchProjectDoesNotCreateSessions(t *testing.T) {
	ts := newPiTestServer(t, nil)

	other := t.TempDir()
	ts.send(map[string]any{"type": "add_project", "sessionPath": other})
	if resp := ts.mustResponse("add_project"); resp["success"] != true {
		t.Fatalf("add_project = %v", resp)
	}
	for i := 0; i < 3; i++ {
		ts.send(map[string]any{"type": "switch_project", "sessionPath": other})
		if resp := ts.mustResponse("switch_project"); resp["success"] != true {
			t.Fatalf("switch_project = %v", resp)
		}
		ts.send(map[string]any{"type": "switch_project", "sessionPath": ts.cwd})
		if resp := ts.mustResponse("switch_project"); resp["success"] != true {
			t.Fatalf("switch_project back = %v", resp)
		}
	}

	ts.send(map[string]any{"type": "list_sessions"})
	list := ts.mustResponse("list_sessions")
	entries, _ := list["data"].(map[string]any)["sessions"].([]any)
	if len(entries) != 0 {
		t.Fatalf("switching project created %d sessions; it should create none: %v", len(entries), list["data"])
	}
}

func TestPiGetModels(t *testing.T) {
	ts := newPiTestServer(t, nil)

	ts.send(map[string]any{"type": "get_models"})
	resp := ts.mustResponse("get_models")

	if resp["success"] != true {
		t.Fatalf("get_models success = %v (error=%v)", resp["success"], resp["error"])
	}
	models, ok := resp["data"].(map[string]any)["models"].([]any)
	if !ok || len(models) == 0 {
		t.Fatalf("models = %#v", resp["data"])
	}
	var sawOpencodeGo, sawOpencodeZen bool
	for _, m := range models {
		entry, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch entry["provider"] {
		case "opencode-go":
			sawOpencodeGo = true
		case "opencode-zen":
			sawOpencodeZen = true
		}
	}
	if !sawOpencodeGo {
		t.Errorf("no opencode-go model in %v", models)
	}
	if !sawOpencodeZen {
		t.Errorf("no opencode-zen model in %v", models)
	}
}

func TestPiGetCommands(t *testing.T) {
	ts := newPiTestServer(t, nil)

	ts.send(map[string]any{"type": "get_commands"})
	resp := ts.mustResponse("get_commands")

	if resp["success"] != true {
		t.Errorf("get_commands success = %v (error=%v)", resp["success"], resp["error"])
	}
}

func TestPiPing(t *testing.T) {
	ts := newPiTestServer(t, nil)

	ts.send(map[string]any{"type": "ping"})
	resp := ts.mustResponse("ping")

	if resp["success"] != true {
		t.Fatalf("ping success = %v (error=%v)", resp["success"], resp["error"])
	}
	if data, ok := resp["data"].(map[string]any); !ok || data["pong"] != true {
		t.Errorf("ping data = %#v, want pong=true", resp["data"])
	}
}

func TestPiUnknownCommand(t *testing.T) {
	ts := newPiTestServer(t, nil)

	ts.send(map[string]any{"type": "bogus_cmd"})
	resp := ts.mustResponse("bogus_cmd")

	if resp["success"] != false {
		t.Errorf("unknown command success = %v, want false", resp["success"])
	}
	if errMsg, _ := resp["error"].(string); errMsg == "" {
		t.Errorf("unknown command error empty: %#v", resp)
	}
}

func TestPiGetEntries(t *testing.T) {
	ts := newPiTestServer(t, nil)

	// A turn first so the session has real entries and a leaf id.
	ts.runPrompt("hello")

	ts.send(map[string]any{"type": "get_entries"})
	resp := ts.mustResponse("get_entries")

	if resp["success"] != true {
		t.Fatalf("get_entries success = %v (error=%v)", resp["success"], resp["error"])
	}
	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("get_entries data = %#v", resp["data"])
	}
	entries, ok := data["entries"].([]any)
	if !ok || len(entries) == 0 {
		t.Errorf("entries = %#v, want non-empty array", data["entries"])
	}
	if leaf, _ := data["leafId"].(string); leaf == "" {
		t.Errorf("leafId empty after prompt: %#v", data)
	}
}

func TestPiGetTree(t *testing.T) {
	ts := newPiTestServer(t, nil)

	ts.runPrompt("hello")

	ts.send(map[string]any{"type": "get_tree"})
	resp := ts.mustResponse("get_tree")

	if resp["success"] != true {
		t.Fatalf("get_tree success = %v (error=%v)", resp["success"], resp["error"])
	}
	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("get_tree data = %#v", resp["data"])
	}
	if _, ok := data["tree"]; !ok {
		t.Errorf("get_tree missing tree key: %#v", data)
	}
	if _, ok := data["leafId"]; !ok {
		t.Errorf("get_tree missing leafId key: %#v", data)
	}
}

func TestPiBash(t *testing.T) {
	ts := newPiTestServer(t, nil)

	ts.send(map[string]any{"type": "bash", "command": "echo hello", "id": "b1"})
	lines := ts.readUntil(func(o map[string]any) bool {
		return o["type"] == "response" && o["command"] == "bash"
	})
	resp := findResponse(lines, "bash")
	if resp == nil {
		t.Fatalf("no bash response; lines: %v", lines)
	}

	var sawUpdate bool
	for _, l := range lines {
		if l["type"] == "bash_execution_update" && l["id"] == "b1" {
			sawUpdate = true
		}
	}
	if !sawUpdate {
		t.Error("no bash_execution_update event with id b1")
	}

	if resp["success"] != true {
		t.Fatalf("bash success = %v (error=%v)", resp["success"], resp["error"])
	}
	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("bash data = %#v", resp["data"])
	}
	if out, _ := data["output"].(string); !strings.Contains(out, "hello") {
		t.Errorf("bash output = %q, want it to contain hello", out)
	}
	if code, _ := data["exitCode"].(float64); code != 0 {
		t.Errorf("bash exitCode = %v, want 0", data["exitCode"])
	}
}

func TestPiSetSessionName(t *testing.T) {
	ts := newPiTestServer(t, nil)

	ts.send(map[string]any{"type": "set_session_name", "name": "mytest"})
	resp := ts.mustResponse("set_session_name")

	if resp["success"] != true {
		t.Errorf("set_session_name success = %v (error=%v)", resp["success"], resp["error"])
	}
}

func TestPiCompact(t *testing.T) {
	ts := newPiTestServer(t, nil)

	ts.runPrompt("hello")

	ts.send(map[string]any{"type": "compact"})
	resp := ts.mustResponse("compact")

	if resp["success"] != true {
		t.Fatalf("compact success = %v (error=%v)", resp["success"], resp["error"])
	}
	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("compact data = %#v", resp["data"])
	}
	if summary, _ := data["summary"].(string); summary == "" {
		t.Errorf("compact summary empty: %#v", data)
	}
}

// A test that adds a project used to write the user's real
// ~/.escape/projects.json, and the entry surfaced later in their sidebar as a
// project they never added, pointing at a temporary directory. The test passed
// and the damage showed up in the app, hours later, in a different repository.
func TestAddingAProjectDoesNotTouchTheRealStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ESCAPE_GLOBAL_DIR", "")

	ts := newPiTestServer(t, nil)
	other := t.TempDir()
	ts.send(map[string]any{"type": "add_project", "sessionPath": other})
	if resp := ts.mustResponse("add_project"); resp["success"] != true {
		t.Fatalf("add_project = %v", resp)
	}

	if _, err := os.Stat(filepath.Join(home, ".escape", "projects.json")); !os.IsNotExist(err) {
		t.Fatalf("the test wrote %s/.escape/projects.json; it must not touch the real one", home)
	}
}
