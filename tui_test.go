package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/ekasc/escape-engine/internal/agent"
	"github.com/ekasc/escape-engine/internal/provider"
	"github.com/ekasc/escape-engine/internal/resources"
	"github.com/ekasc/escape-engine/internal/session"
	"github.com/ekasc/escape-engine/internal/settings"
)

func TestTUIInputEditingAndHistory(t *testing.T) {
	m := newTUIModel(nil, nil, "model", "off")
	m.input.InsertString("hello")
	m.input.CursorStart()
	m.input.InsertRune('>')
	if got := m.input.Value(); got != ">hello" {
		t.Fatalf("input = %q", got)
	}
	m.history = []string{"one", "two"}
	m.historyPrevious()
	if got := m.input.Value(); got != "two" {
		t.Fatalf("history input = %q", got)
	}
	m.historyNext()
	if got := m.input.Value(); got != "" {
		t.Fatalf("history next = %q", got)
	}
}

func TestTUICommandCompletionIncludesReview(t *testing.T) {
	m := newTUIModel(nil, nil, "model", "off")
	m.input.SetValue("/perm")
	m.updateSuggestions()
	if len(m.suggestions) != 1 || m.suggestions[0].name != "/permissions" {
		t.Fatalf("suggestions = %+v", m.suggestions)
	}
	m.input.SetValue("/ren")
	m.updateSuggestions()
	if len(m.suggestions) != 1 || m.suggestions[0].name != "/rename" {
		t.Fatalf("rename suggestions = %+v", m.suggestions)
	}
	m.input.SetValue("/recap")
	m.updateSuggestions()
	if len(m.suggestions) != 1 || m.suggestions[0].name != "/recap" {
		t.Fatalf("recap suggestions = %+v", m.suggestions)
	}
	m.runCommand("/permissions")
	if !strings.Contains(strings.Join(m.lines, "\n"), "permissions: auto") {
		t.Fatalf("permissions line missing: %+v", m.lines)
	}
	prompt := tuiReviewPromptFor("main.go", "internal/agent")
	if !strings.Contains(prompt, "Focus on these paths: main.go, internal/agent.") {
		t.Fatalf("scoped review prompt = %q", prompt)
	}
	m.input.SetValue("/rev")
	m.updateSuggestions()
	if len(m.suggestions) != 1 || m.suggestions[0].name != "/review" {
		t.Fatalf("suggestions = %+v", m.suggestions)
	}
	if cmd := m.runCommand("/review"); cmd == nil || !m.busy {
		t.Fatal("/review did not start a review turn")
	}
}

func TestTUICommandCompletionIncludesFollowup(t *testing.T) {
	m := newTUIModel(nil, nil, "model", "off")
	m.input.SetValue("/follow")
	m.updateSuggestions()
	if len(m.suggestions) != 1 || m.suggestions[0].name != "/followup" {
		t.Fatalf("suggestions = %+v", m.suggestions)
	}
}

func TestTUIResourceCommandCompletion(t *testing.T) {
	cwd := t.TempDir()
	prompts := filepath.Join(cwd, ".escape", "prompts")
	if err := os.MkdirAll(prompts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prompts, "ship.md"), []byte("---\ndescription: Ship it\nargument-hint: \"<target>\"\n---\nship the change"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newTUIModel(nil, nil, "model", "off")
	m.resourceLoader = resources.New(cwd, settings.Defaults())
	m.input.SetValue("/ship")
	m.updateSuggestions()
	if len(m.suggestions) != 1 || m.suggestions[0].name != "/ship" || m.suggestions[0].argumentHint != "<target>" {
		t.Fatalf("resource suggestions = %+v", m.suggestions)
	}
	if view := m.commandView(); !strings.Contains(view, "/ship <target>") {
		t.Fatalf("command view = %q", view)
	}
}

func TestTUIResourceSkillCompletion(t *testing.T) {
	cwd := t.TempDir()
	skillDir := filepath.Join(cwd, ".escape", "skills", "demo")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: demo\ndescription: Demo skill.\n---\nUse the demo workflow."
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	loader := resources.New(cwd, settings.Defaults())
	m := newTUIModel(nil, nil, "model", "off")
	m.resourceLoader = loader
	m.input.SetValue("/skill:dem")
	m.updateSuggestions()
	if len(m.suggestions) != 1 || m.suggestions[0].name != "/skill:demo" {
		t.Fatalf("skill suggestions = %+v", m.suggestions)
	}
	expanded, ok := expandResourceCommand(loader, "/skill:demo now")
	if !ok || !strings.Contains(expanded, "User: now") {
		t.Fatalf("skill expansion = %q, ok=%v", expanded, ok)
	}
}

func TestTUICommandsInventory(t *testing.T) {
	m := newTUIModel(nil, nil, "model", "off")
	m.listAllCommands()
	if !strings.Contains(strings.Join(m.lines, "\n"), "/commands") || !strings.Contains(strings.Join(m.lines, "\n"), "/help") {
		t.Fatalf("command inventory = %+v", m.lines)
	}
}

func TestTUIAssistantMarkdownRendering(t *testing.T) {
	entries := []session.Entry{{Type: session.TypeMessage, Message: &session.Message{
		Role:    session.RoleAssistant,
		Content: []session.Block{{Type: session.BlockText, Text: "# Result\n\n**done**"}},
	}}}
	transcript := strings.Join(transcriptLines(entries, 80), "\n")
	if strings.Contains(transcript, "**done**") || !strings.Contains(transcript, "Result") || !strings.Contains(transcript, "done") {
		t.Fatalf("assistant markdown was not rendered:\n%s", transcript)
	}
}

func TestTUICommandSuggestionsTrackTypedValue(t *testing.T) {
	m := newTUIModel(nil, nil, "model", "off")
	for _, r := range "/commands" {
		updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Text: string(r), Code: r}))
		m = updated.(*tuiModel)
	}
	if len(m.suggestions) != 1 || m.suggestions[0].name != "/commands" {
		t.Fatalf("suggestions for /commands = %+v", m.suggestions)
	}
}

func TestTUICommandCompletion(t *testing.T) {
	m := newTUIModel(nil, nil, "model", "off")
	m.input.SetValue("/s")
	m.updateSuggestions()
	if len(m.suggestions) != 5 {
		t.Fatalf("suggestions = %+v", m.suggestions)
	}
	m.completeCommand()
	if got := m.input.Value(); got != "/state" {
		t.Fatalf("first completion = %q", got)
	}
	m.completeCommand()
	if got := m.input.Value(); got != "/status" {
		t.Fatalf("second completion = %q", got)
	}
}

func TestTUIHydratesSessionTranscript(t *testing.T) {
	entries := []session.Entry{
		{Type: session.TypeSessionInfo, Name: "Fix the failing test"},
		{Type: session.TypeMessage, Message: &session.Message{Role: session.RoleUser, Content: []session.Block{{Type: session.BlockText, Text: "inspect the failure"}}}},
		{Type: session.TypeMessage, Message: &session.Message{Role: session.RoleAssistant, Content: []session.Block{
			{Type: session.BlockText, Text: "I found the failing assertion."},
			{Type: session.BlockToolCall, Name: "read", Arguments: map[string]any{"path": "main.go"}},
		}}},
		{Type: session.TypeMessage, Message: &session.Message{Role: session.RoleToolResult, Content: []session.Block{{Type: session.BlockText, Text: "package main"}}, ToolName: "read"}},
	}
	m := newTUIModel(nil, nil, "model", "off", entries)
	transcript := m.transcript.GetContent()
	for _, want := range []string{"session: Fix the failing test", "inspect the failure", "I found the", "assertion.", "[tool] read", "[result] package main"} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("transcript missing %q:\n%s", want, transcript)
		}
	}
	if got := strings.Count(transcript, "package main"); got != 1 {
		t.Fatalf("tool result rendered %d times, want once:\n%s", got, transcript)
	}
}

func TestTUIAssistantStreamUsesDedicatedLine(t *testing.T) {
	m := newTUIModel(nil, make(chan agent.Event), "model", "off")
	updated, _ := m.Update(tuiEventMsg{event: agent.Event{Event: agent.EventMessageStart, Role: session.RoleAssistant}})
	got := updated.(*tuiModel)
	updated, _ = got.Update(tuiEventMsg{event: agent.Event{Event: agent.EventMessageDelta, Text: "hello"}})
	got = updated.(*tuiModel)
	updated, _ = got.Update(tuiEventMsg{event: agent.Event{Event: agent.EventToolCall, Name: "bash", Args: map[string]any{"command": "go test ./..."}}})
	got = updated.(*tuiModel)
	updated, _ = got.Update(tuiEventMsg{event: agent.Event{Event: agent.EventMessageDelta, Text: " world"}})
	got = updated.(*tuiModel)
	if got.lines[len(got.lines)-2] != "hello world" {
		t.Fatalf("assistant line was overwritten: %+v", got.lines)
	}
}

func TestTUIQueuesWhileBusy(t *testing.T) {
	store, err := session.Open(filepath.Join(t.TempDir(), "session.jsonl"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Append(session.Entry{Type: session.TypeSessionInfo, Name: "queued test"}); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	ag := agent.New(agent.Options{
		Store: store,
		Provider: &provider.Fake{Handler: func(ctx context.Context, _ provider.Request) ([]provider.Event, error) {
			if calls.Add(1) == 1 {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return []provider.Event{{Kind: provider.EventDone, StopReason: "stop"}}, nil
		}},
		Cwd: t.TempDir(),
	})
	events := ag.Events()
	defer ag.Unsubscribe(events)
	if _, err := ag.Send("first prompt"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("agent did not start")
	}

	m := newTUIModel(ag, events, "model", "off")
	m.busy = true
	if cmd := m.submit("follow the current plan"); cmd != nil {
		t.Fatal("queued submission unexpectedly returned a command")
	}
	if len(m.history) != 1 || m.history[0] != "follow the current plan" {
		t.Fatalf("history = %+v", m.history)
	}
	close(release)
	settled := 0
	deadline := time.After(2 * time.Second)
	for settled < 2 {
		select {
		case event := <-events:
			if event.Event == agent.EventSettled {
				settled++
			}
		case <-deadline:
			t.Fatalf("settled events = %d, want 2", settled)
		}
	}
}

func TestTUIListSessions(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	// A session has to have something in it to exist; opening a store creates
	// nothing on disk.
	path := filepath.Join(root, "project", "session.jsonl")
	store, err := session.Open(path, cwd)
	if err != nil {
		t.Fatal(err)
	}
	// A non-message entry, so the session exists without picking up a title from
	// a user message — "(untitled)" is the label under test.
	if _, err := store.Append(session.Entry{Type: session.TypeModelChange, Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ESCAPE_SESSIONS_DIR", root)
	m := newTUIModel(nil, nil, "model", "off")
	m.cwd = cwd
	m.listSessions()
	if len(m.sessionItems) != 1 || !strings.Contains(strings.Join(m.lines, "\n"), "1. (untitled)") {
		t.Fatalf("sessions = %+v, lines = %+v", m.sessionItems, m.lines)
	}
}

func TestTUIEmptyFollowupShowsUsage(t *testing.T) {
	m := newTUIModel(nil, nil, "model", "off")
	m.runCommand("/followup")
	if !strings.Contains(strings.Join(m.lines, "\n"), "usage: /followup <text>") {
		t.Fatalf("lines = %+v", m.lines)
	}
}

func TestUndoSessionPath(t *testing.T) {
	cwd := t.TempDir()
	store, err := session.Open(filepath.Join(t.TempDir(), "source.jsonl"), cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	appendMessage := func(role, text string) {
		t.Helper()
		_, err := store.Append(session.Entry{Type: session.TypeMessage, Message: &session.Message{Role: role, Content: []session.Block{{Type: session.BlockText, Text: text}}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	appendMessage(session.RoleUser, "first")
	appendMessage(session.RoleAssistant, "first response")
	appendMessage(session.RoleUser, "second")
	appendMessage(session.RoleAssistant, "second response")

	m := newTUIModel(nil, nil, "model", "off")
	m.store = store
	m.cwd = cwd
	m.listForkPoints()
	if !strings.Contains(strings.Join(m.lines, "\n"), "1. first") || !strings.Contains(strings.Join(m.lines, "\n"), "2. second") {
		t.Fatalf("fork point listing = %+v", m.lines)
	}

	newPath, err := undoSessionPath(store.Path(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := session.ReadAll(newPath)
	if err != nil {
		t.Fatal(err)
	}
	var text []string
	for _, entry := range entries {
		if entry.Message != nil {
			text = append(text, entry.Message.Text(false))
		}
	}
	if strings.Join(text, ",") != "first,first response" {
		t.Fatalf("undo branch messages = %v", text)
	}
	forkPath, err := forkSessionPath(store.Path(), cwd, 0)
	if err != nil {
		t.Fatal(err)
	}
	forkEntries, err := session.ReadAll(forkPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(forkEntries) != 2 || forkEntries[1].Message == nil || forkEntries[1].Message.Text(false) != "first" {
		t.Fatalf("fork entries = %+v", forkEntries)
	}

	oneMessageStore, err := session.Open(filepath.Join(t.TempDir(), "one.jsonl"), cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer oneMessageStore.Close()
	appendMessage = func(role, text string) {
		t.Helper()
		_, err := oneMessageStore.Append(session.Entry{Type: session.TypeMessage, Message: &session.Message{Role: role, Content: []session.Block{{Type: session.BlockText, Text: text}}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	appendMessage(session.RoleUser, "only")
	freshPath, err := undoSessionPath(oneMessageStore.Path(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	// Undoing the only turn leaves nothing: with no parent to fork from, this is
	// just a path, and a path with nothing written to it is not a session. It
	// used to be an empty file on disk, which the shell then listed.
	if _, err := os.Stat(freshPath); !os.IsNotExist(err) {
		t.Fatalf("undoing the first turn created %s; it should create nothing", freshPath)
	}
}

func TestTUIClearTranscript(t *testing.T) {
	m := newTUIModel(nil, nil, "model", "off")
	m.lines = append(m.lines, "old transcript")
	m.runCommand("/clear")
	if len(m.lines) != 2 || !strings.Contains(m.lines[0], "escape personal agent") {
		t.Fatalf("cleared lines = %+v", m.lines)
	}
	m.busy = true
	m.lines = append(m.lines, "keep")
	m.runCommand("/clear")
	if !strings.Contains(strings.Join(m.lines, "\n"), "keep") {
		t.Fatal("busy transcript was cleared")
	}
}

func TestTUIQuestionEvent(t *testing.T) {
	m := newTUIModel(nil, make(chan agent.Event), "model", "off")
	updated, _ := m.Update(tuiEventMsg{event: agent.Event{Event: agent.EventQuestionRequested, ToolCallID: "q1", Question: "Continue?"}})
	got := updated.(*tuiModel)
	if got.questionID != "q1" {
		t.Fatalf("question id = %q", got.questionID)
	}
	if !strings.Contains(strings.Join(got.lines, "\n"), "Continue?") {
		t.Fatal("question was not rendered")
	}
}

func TestTUIQueueUpdateRenders(t *testing.T) {
	m := newTUIModel(nil, make(chan agent.Event), "model", "off")
	updated, _ := m.Update(tuiEventMsg{event: agent.Event{Event: agent.EventQueueUpdate, Steering: []string{"inspect tests"}}})
	got := updated.(*tuiModel)
	if !strings.Contains(strings.Join(got.lines, "\n"), "[queued] steering: inspect tests") {
		t.Fatalf("queue update was not rendered: %+v", got.lines)
	}
}

func TestTUIApprovalPrompt(t *testing.T) {
	m := newTUIModel(nil, make(chan agent.Event), "model", "off")
	updated, _ := m.Update(tuiEventMsg{event: agent.Event{
		Event:      agent.EventApprovalRequested,
		ToolCallID: "call_1",
		Name:       "bash",
		Args:       map[string]any{"command": "go test ./..."},
	}})
	got := updated.(*tuiModel)
	if got.approvalID != "call_1" {
		t.Fatalf("approval id = %q", got.approvalID)
	}
	if !strings.Contains(strings.Join(got.lines, "\n"), "[approval] bash") || !strings.Contains(strings.Join(got.lines, "\n"), "(y/n)") {
		t.Fatalf("approval prompt missing: %+v", got.lines)
	}
}

func TestTUIViewStaysWithinTerminal(t *testing.T) {
	m := newTUIModel(nil, nil, "model", "off")
	updated, _ := m.Update(tea.WindowSizeMsg{Height: 12, Width: 40})
	m = updated.(*tuiModel)
	for i := 0; i < 40; i++ {
		m.lines = append(m.lines, "line")
	}
	m.refreshTranscript(false)
	view := m.View()
	if got := lipgloss.Height(view.Content); got > 12 {
		t.Fatalf("view height = %d, want <= 12\n%s", got, view.Content)
	}
	if !m.input.VirtualCursor() {
		t.Fatal("expected the textarea to render its own cursor")
	}
	if view.Cursor != nil {
		t.Fatal("virtual cursor should not also set a top-level cursor")
	}
}

func TestTUICommandSuggestionsDoNotCoverComposer(t *testing.T) {
	m := newTUIModel(nil, nil, "model", "off")
	updated, _ := m.Update(tea.WindowSizeMsg{Height: 12, Width: 40})
	m = updated.(*tuiModel)
	m.input.SetValue("/e")
	m.updateSuggestions()

	view := m.View()
	if got := lipgloss.Height(view.Content); got > 12 {
		t.Fatalf("view height with suggestions = %d, want <= 12\n%s", got, view.Content)
	}
	if !strings.Contains(view.Content, "/export") || !strings.Contains(view.Content, "/exit") {
		t.Fatalf("suggestions missing from view:\n%s", view.Content)
	}
}

func TestTUIViewRefreshesStaleSuggestions(t *testing.T) {
	m := newTUIModel(nil, nil, "model", "off")
	m.input.SetValue("/commands")
	view := m.View()
	if !strings.Contains(view.Content, "/commands") || strings.Contains(view.Content, "/login") {
		t.Fatalf("stale suggestions rendered:\n%s", view.Content)
	}
}

func TestTUICommandDockDoesNotMoveComposer(t *testing.T) {
	m := newTUIModel(nil, nil, "model", "off")
	updated, _ := m.Update(tea.WindowSizeMsg{Height: 20, Width: 80})
	m = updated.(*tuiModel)
	without := m.View().Content
	m.input.SetValue("/e")
	with := m.View().Content
	needle := "Ask escape to build"
	if strings.Index(without, needle) != strings.Index(with, needle) {
		t.Fatalf("composer moved when command dock appeared:\nwithout=%d\nwith=%d", strings.Index(without, needle), strings.Index(with, needle))
	}
}

func TestTUIOpenLoginForm(t *testing.T) {
	m := newTUIModel(nil, nil, "model", "off")
	if cmd := m.runCommand("/login"); cmd == nil || m.loginForm == nil {
		t.Fatal("/login did not open the embedded provider form")
	}
	view := m.View()
	if !strings.Contains(view.Content, "Choose a provider") {
		t.Fatalf("login form is not visible:\n%s", view.Content)
	}
}
