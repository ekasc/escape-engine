package memory_test

import (
	"strings"
	"testing"

	"github.com/ekasc/escape/engine/internal/memory"
)

// The contract that makes durable memory useful: a write during a session is on
// disk immediately, but the rendered snapshot the agent already holds is
// unchanged, so the prompt prefix stays cacheable. The next session picks it up.
func TestWriteAfterRenderIsNotInTheOldSnapshot(t *testing.T) {
	root, cwd := t.TempDir(), "/repo/escape"
	store := memory.Open(root, cwd)

	if err := store.Add("decided in session one"); err != nil {
		t.Fatal(err)
	}
	first := store.Render()
	if !strings.Contains(first, "decided in session one") {
		t.Fatalf("first snapshot missing the entry:\n%s", first)
	}

	// A later write, as if mid-session.
	if err := store.Add("learned later in the same session"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first, "learned later") {
		t.Fatal("the already-rendered snapshot must not change")
	}

	// A new session re-reads from disk and sees both.
	next := memory.Open(root, cwd).Render()
	if !strings.Contains(next, "decided in session one") || !strings.Contains(next, "learned later") {
		t.Fatalf("next session snapshot is stale:\n%s", next)
	}
}
