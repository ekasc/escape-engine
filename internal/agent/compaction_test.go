package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekasc/escape-engine/internal/provider"
	"github.com/ekasc/escape-engine/internal/session"
)

// summarizeHandler is a fake provider handler that returns a fixed structured
// summary for compaction/branch-summary meta-calls.
func summarizeHandler(ctx context.Context, req provider.Request) ([]provider.Event, error) {
	return []provider.Event{
		{Kind: provider.EventText, Text: "## Goal\nSummarize test session.\n## Next Steps\n1. Keep going.\n"},
		{Kind: provider.EventDone, StopReason: "stop"},
	}, nil
}

func appendMsg(t *testing.T, store *session.Store, role string, blocks ...session.Block) session.Entry {
	t.Helper()
	e, err := store.Append(session.Entry{Type: session.TypeMessage, Message: &session.Message{Role: role, Content: blocks}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestFindCutPointNeverCutsToolResult(t *testing.T) {
	entries := []session.Entry{
		{Type: session.TypeMessage, Message: &session.Message{Role: session.RoleUser, Content: []session.Block{{Type: session.BlockText, Text: strings.Repeat("x", 1000)}}}},
		{Type: session.TypeMessage, Message: &session.Message{Role: session.RoleAssistant, Content: []session.Block{{Type: session.BlockToolCall, ID: "c1", Name: "read", Arguments: map[string]any{"path": "/a"}}}}},
		{Type: session.TypeMessage, Message: &session.Message{Role: session.RoleToolResult, Content: []session.Block{{Type: session.BlockText, Text: strings.Repeat("y", 2000)}}}},
		{Type: session.TypeMessage, Message: &session.Message{Role: session.RoleUser, Content: []session.Block{{Type: session.BlockText, Text: "next question"}}}},
	}
	cut := findCutPoint(entries, 100) // small budget forces retreat to a user message
	if cut < 0 || cut >= len(entries) {
		t.Fatalf("cut out of range: %d", cut)
	}
	if entries[cut].Message.Role != session.RoleUser {
		t.Fatalf("cut at %d is not a user message: %+v", cut, entries[cut])
	}
}

func TestSerializeConversation(t *testing.T) {
	entries := []session.Entry{
		{Type: session.TypeMessage, Message: &session.Message{Role: session.RoleUser, Content: []session.Block{{Type: session.BlockText, Text: "hello"}}}},
		{Type: session.TypeMessage, Message: &session.Message{Role: session.RoleAssistant, Content: []session.Block{
			{Type: session.BlockText, Text: "hi there"},
			{Type: session.BlockToolCall, ID: "c1", Name: "read", Arguments: map[string]any{"path": "x.go"}},
		}}},
		{Type: session.TypeMessage, Message: &session.Message{Role: session.RoleToolResult, Content: []session.Block{{Type: session.BlockText, Text: "file contents"}}}},
	}
	out := serializeConversation(entries)
	for _, want := range []string{"[User]: hello", "[Assistant]: hi there", "[Assistant tool calls]: read(", "[Tool result]: file contents"} {
		if !strings.Contains(out, want) {
			t.Fatalf("serializeConversation missing %q:\n%s", want, out)
		}
	}
}

func TestCollectFiles(t *testing.T) {
	entries := []session.Entry{
		{Type: session.TypeMessage, Message: &session.Message{Role: session.RoleAssistant, Content: []session.Block{
			{Type: session.BlockToolCall, Name: "read", Arguments: map[string]any{"path": "a.go"}},
			{Type: session.BlockToolCall, Name: "edit", Arguments: map[string]any{"path": "b.go"}},
			{Type: session.BlockToolCall, Name: "write", Arguments: map[string]any{"path": "c.go"}},
		}}},
	}
	reads := collectReadFiles(entries)
	if len(reads) != 1 || reads[0] != "a.go" {
		t.Fatalf("reads = %v", reads)
	}
	mods := collectModifiedFiles(entries)
	if len(mods) != 2 {
		t.Fatalf("mods = %v", mods)
	}
}

func TestContextWindowFor(t *testing.T) {
	if contextWindowFor("gpt-5.6-luna") != 272000 {
		t.Fatal("gpt-5.6-luna window wrong")
	}
	if contextWindowFor("unknown-model") != DefaultContextWindow {
		t.Fatal("default window wrong")
	}
	// The fallback must sit below the windows of the models most sessions run
	// on. Guessing high does not merely compact late, it sends a request the
	// provider rejects.
	if DefaultContextWindow >= 200000 {
		t.Fatalf("default window %d is optimistic; an unknown model then fails instead of compacting", DefaultContextWindow)
	}
}

func TestCompactAppendsEntry(t *testing.T) {
	ag, store, _ := newTestAgent(t, summarizeHandler)
	appendMsg(t, store, session.RoleUser, session.Block{Type: session.BlockText, Text: "do a thing"})
	appendMsg(t, store, session.RoleAssistant, session.Block{Type: session.BlockText, Text: "done"})

	res, err := ag.Compact(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary == "" {
		t.Fatal("empty summary")
	}
	entries, _ := session.ReadAll(store.Path())
	var found *session.Entry
	for i := range entries {
		if entries[i].Type == session.TypeCompaction {
			found = &entries[i]
		}
	}
	if found == nil {
		t.Fatal("no compaction entry appended")
	}
	if found.Summary != res.Summary || found.TokensBefore != res.TokensBefore {
		t.Fatalf("compaction entry mismatch: %+v vs %+v", found, res)
	}
}

func TestSnapcompactRendersPNGFrames(t *testing.T) {
	raw, err := buildSnapArchive("USER: hello\nASSISTANT: world")
	if err != nil {
		t.Fatal(err)
	}
	images, fallback, err := snapArchiveImages(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) == 0 || fallback == "" {
		t.Fatalf("images=%d fallback=%q", len(images), fallback)
	}
	if string(images[0].Data[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("first frame is not PNG")
	}
}

func TestSnapcompactArchivesWithoutProvider(t *testing.T) {
	ag, store, _ := newTestAgent(t, func(context.Context, provider.Request) ([]provider.Event, error) {
		t.Fatal("snapcompact must not call the provider")
		return nil, nil
	})
	appendMsg(t, store, session.RoleUser, session.Block{Type: session.BlockText, Text: "old context"})
	appendMsg(t, store, session.RoleUser, session.Block{Type: session.BlockText, Text: strings.Repeat("old context ", 10000)})
	appendMsg(t, store, session.RoleAssistant, session.Block{Type: session.BlockText, Text: "recent"})

	res, err := ag.Snapcompact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.FirstKeptEntryID == "" {
		t.Fatal("no retained entry")
	}
	entries, _ := session.ReadAll(store.Path())
	var archived bool
	for _, e := range entries {
		if e.Type == session.TypeCompaction && e.SnapcompactData != "" && e.FromHook {
			if details, ok := e.Details.(map[string]any); ok {
				if details["snapcompactGeneration"] != nil && details["snapcompactProfile"] != nil {
					archived = true
				}
			}
		}
	}
	if !archived {
		t.Fatalf("snapcompact archive not persisted: %+v", entries)
	}
}

func TestSummarizeBranch(t *testing.T) {
	ag, store, _ := newTestAgent(t, summarizeHandler)
	root := appendMsg(t, store, session.RoleUser, session.Block{Type: session.BlockText, Text: "start"})
	a1 := appendMsg(t, store, session.RoleAssistant, session.Block{Type: session.BlockText, Text: "branch A"})
	// branch B diverges from root (same parent as a1)
	appendMsg(t, store, session.RoleAssistant, session.Block{Type: session.BlockText, Text: "branch B"})
	// append a child of a1 (on branch A) so the branch has content
	appendMsg(t, store, session.RoleToolResult, session.Block{Type: session.BlockText, Text: "tool result on branch A"})

	if err := ag.SummarizeBranch(context.Background(), a1.ID, root.ID); err != nil {
		t.Fatal(err)
	}
	entries, _ := session.ReadAll(store.Path())
	var found *session.Entry
	for i := range entries {
		if entries[i].Type == session.TypeBranchSummary {
			found = &entries[i]
		}
	}
	if found == nil {
		t.Fatal("no branch_summary appended")
	}
	if found.FromID != a1.ID {
		t.Fatalf("branch summary fromID = %q, want %q", found.FromID, a1.ID)
	}
}

// silence unused-import guard for filepath (used via newTestAgent signature elsewhere)
var _ = filepath.Join
