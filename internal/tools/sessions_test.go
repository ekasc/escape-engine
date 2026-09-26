package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekasc/escape/engine/internal/session"
)

// writeSession writes a real session file through the store, so the fixture
// matches the on-disk shape ReadInfo expects: the name lives in a separate
// session_info entry, and the first user text in a message entry.
func writeSession(t *testing.T, root, cwd, name, first string) string {
	t.Helper()
	dir := filepath.Join(root, session.SlugForDir(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, time.Now().Format("2006-01-02T15-04-05-000")+"_"+session.NewSessionID()+".jsonl")
	store, err := session.Open(path, cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if name != "" {
		if _, err := store.Append(session.Entry{Type: session.TypeSessionInfo, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if first != "" {
		if _, err := store.Append(session.Entry{
			Type: session.TypeMessage,
			Message: &session.Message{
				Role:    session.RoleUser,
				Content: []session.Block{{Type: "text", Text: first}},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func newSessionsTool(t *testing.T, root, cwd, current string) (Tool, *session.Control) {
	t.Helper()
	ctrl := &session.Control{}
	ctrl.SetCurrent(current)
	return Sessions(root, cwd, ctrl.Current, ctrl), ctrl
}

func TestSessionsSearchFindsByFirstMessage(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	writeSession(t, root, cwd, "RPC work", "fix the provider session header binding")
	writeSession(t, root, cwd, "Other", "unrelated notes about styling")

	tool, _ := newSessionsTool(t, root, cwd, "")
	res := tool.Run(context.Background(), map[string]any{"op": "search", "query": "session header"})
	if res.IsError {
		t.Fatalf("search: %s", res.Output)
	}
	if !strings.Contains(res.Output, "RPC work") {
		t.Fatalf("expected the matching session:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "Other") {
		t.Fatalf("search returned a non-match:\n%s", res.Output)
	}
	// The model needs the id to resume it, and it must actually be resumable.
	id := fieldAfter(res.Output, "id: ")
	if id == "" {
		t.Fatalf("search must expose an id to resume:\n%s", res.Output)
	}
	_, ctrl := newSessionsTool(t, root, cwd, "")
	resume := Sessions(root, cwd, ctrl.Current, ctrl).Run(
		context.Background(), map[string]any{"op": "resume", "session": id})
	if resume.IsError {
		t.Fatalf("the id from search should be resumable: %s", resume.Output)
	}
	if ctrl.TakeResume() == "" {
		t.Fatal("resume by the searched id queued nothing")
	}
}

func TestSessionsListShowsMostRecent(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	writeSession(t, root, cwd, "First", "one")
	writeSession(t, root, cwd, "Second", "two")

	tool, _ := newSessionsTool(t, root, cwd, "")
	res := tool.Run(context.Background(), map[string]any{"op": "list"})
	if res.IsError {
		t.Fatalf("list: %s", res.Output)
	}
	if !strings.Contains(res.Output, "First") || !strings.Contains(res.Output, "Second") {
		t.Fatalf("list should show both:\n%s", res.Output)
	}
}

func TestSessionsSearchIsScopedToTheProject(t *testing.T) {
	root := t.TempDir()
	mine, other := filepath.Join(root, "mine"), filepath.Join(root, "other")
	writeSession(t, root, mine, "Mine", "session header here")
	writeSession(t, root, other, "Theirs", "session header there")

	tool, _ := newSessionsTool(t, root, mine, "")
	res := tool.Run(context.Background(), map[string]any{"op": "search", "query": "session header"})
	if strings.Contains(res.Output, "Theirs") {
		t.Fatalf("search leaked another project:\n%s", res.Output)
	}
}

// The whole point of the deferral: a resume requested from inside a turn must
// only be recorded, never applied, because applying it would stop the agent
// running this very tool.
func TestSessionsResumeIsDeferredNotApplied(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	target := writeSession(t, root, cwd, "Old work", "earlier")
	_, ctrl := newSessionsTool(t, root, cwd, "")

	tool := Sessions(root, cwd, ctrl.Current, ctrl)
	res := tool.Run(context.Background(), map[string]any{"op": "resume", "session": filepath.Base(target)})
	if res.IsError {
		t.Fatalf("resume: %s", res.Output)
	}
	if got := ctrl.TakeResume(); got != target {
		t.Fatalf("resume should be recorded as %q, got %q", target, got)
	}
	// Taking it clears it, so it is applied exactly once.
	if got := ctrl.TakeResume(); got != "" {
		t.Fatalf("resume should be consumed once, got %q a second time", got)
	}
	// The tool must not claim the session already switched.
	if !strings.Contains(res.Output, "after this turn") {
		t.Fatalf("tool should say the switch is deferred:\n%s", res.Output)
	}
}

func TestSessionsResumeCurrentIsANoop(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	current := writeSession(t, root, cwd, "Current", "here")
	ctrl := &session.Control{}
	ctrl.SetCurrent(current)

	tool := Sessions(root, cwd, ctrl.Current, ctrl)
	res := tool.Run(context.Background(), map[string]any{"op": "resume", "session": filepath.Base(current)})
	if res.IsError {
		t.Fatalf("resuming the current session should not error: %s", res.Output)
	}
	if got := ctrl.TakeResume(); got != "" {
		t.Fatalf("resuming the current session should not queue a switch, got %q", got)
	}
}

func TestSessionsResumeRejectsUnknown(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	writeSession(t, root, cwd, "Known", "x")
	tool, _ := newSessionsTool(t, root, cwd, "")

	res := tool.Run(context.Background(), map[string]any{"op": "resume", "session": "nope"})
	if !res.IsError {
		t.Fatal("resuming an unknown session should fail")
	}
}

func TestSessionsResumeNeedsAnArgument(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	tool, _ := newSessionsTool(t, root, cwd, "")
	res := tool.Run(context.Background(), map[string]any{"op": "resume"})
	if !res.IsError {
		t.Fatal("resume without a session should fail")
	}
}

func TestSessionsSearchNoMatchSaysSo(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	writeSession(t, root, cwd, "Known", "something")
	tool, _ := newSessionsTool(t, root, cwd, "")

	res := tool.Run(context.Background(), map[string]any{"op": "search", "query": "zzzznotpresent"})
	if res.IsError {
		t.Fatalf("no match is not an error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "no sessions") {
		t.Fatalf("should report no matches:\n%s", res.Output)
	}
}

func TestSessionsBadOpIsRejected(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	tool, _ := newSessionsTool(t, root, cwd, "")
	res := tool.Run(context.Background(), map[string]any{"op": "teleport"})
	if !res.IsError {
		t.Fatal("an unknown op should fail")
	}
}

// fieldAfter pulls a labelled value out of the tool's plain-text output.
func fieldAfter(out, label string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), label) {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), label))
		}
	}
	return ""
}
