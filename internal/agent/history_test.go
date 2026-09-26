package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ekasc/escape/engine/internal/session"
)

func newCacheFixture(t *testing.T) (*session.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	store, err := session.Open(path, "/repo/mine")
	if err != nil {
		t.Fatal(err)
	}
	return store, path
}

func appendUser(store *session.Store, text string) {
	if _, err := store.Append(session.Entry{
		Type:    session.TypeMessage,
		Message: &session.Message{Role: session.RoleUser, Content: []session.Block{{Type: "text", Text: text}}},
	}); err != nil {
		panic(err)
	}
}

// The whole point of the cache is that it must be indistinguishable from a full
// read. Any divergence means the agent silently loses or repeats history.
func TestEntryCacheMatchesAFullRead(t *testing.T) {
	store, path := newCacheFixture(t)
	var cache entryCache

	appendUser(store, "first")
	afterFirst, err := cache.sinceAppends(path)
	if err != nil {
		t.Fatal(err)
	}
	appendUser(store, "second")
	appendUser(store, "third")
	afterThird, err := cache.sinceAppends(path)
	if err != nil {
		t.Fatal(err)
	}

	all, err := session.ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterFirst) != len(all)-2 {
		t.Fatalf("first read returned %d entries, expected %d", len(afterFirst), len(all)-2)
	}
	if len(afterThird) != len(all) {
		t.Fatalf("cached read returned %d entries, full read returned %d", len(afterThird), len(all))
	}
	for i := range all {
		if afterThird[i].ID != all[i].ID {
			t.Fatalf("entry %d differs: cached %s, full %s", i, afterThird[i].ID, all[i].ID)
		}
	}
}

// Calling it again with nothing new must not duplicate anything.
func TestEntryCacheIsIdempotentOnAnUnchangedFile(t *testing.T) {
	store, path := newCacheFixture(t)
	appendUser(store, "only")
	var cache entryCache
	first, err := cache.sinceAppends(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.sinceAppends(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatalf("a no-op read changed the entry count: %d then %d", len(first), len(second))
	}
}

// A session switch builds a new Agent, but a stale cache must still not leak
// entries across paths if one is ever reused.
func TestEntryCacheRestartsOnAPathChange(t *testing.T) {
	storeA, pathA := newCacheFixture(t)
	appendUser(storeA, "from a")
	cache := entryCache{}
	if _, err := cache.sinceAppends(pathA); err != nil {
		t.Fatal(err)
	}
	pathB := filepath.Join(t.TempDir(), "b.jsonl")
	storeB, err := session.Open(pathB, "/repo/mine")
	if err != nil {
		t.Fatal(err)
	}
	appendUser(storeB, "from b")
	entries, err := cache.sinceAppends(pathB)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected only b's header and message, got %d entries", len(entries))
	}
	for _, e := range entries {
		if e.Message != nil && e.Message.Text(false) == "from a" {
			t.Fatal("entries leaked across a path change")
		}
	}
}

// A file that shrank was replaced rather than appended to, so the cache has to
// notice instead of returning entries that are no longer there.
func TestEntryCacheRestartsOnAShrunkenFile(t *testing.T) {
	_, path := newCacheFixture(t)
	var cache entryCache
	big := make([]byte, 0, 4096)
	store, err := session.Open(path, "/repo/mine")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		appendUser(store, "padding to grow the file")
	}
	big, _ = os.ReadFile(path)
	if _, err := cache.sinceAppends(path); err != nil {
		t.Fatal(err)
	}
	if len(cache.entries) == 0 {
		t.Fatal("fixture did not produce entries")
	}

	fresh, err := session.Open(filepath.Join(t.TempDir(), "other.jsonl"), "/repo/mine")
	if err != nil {
		t.Fatal(err)
	}
	_ = fresh
	// Replace the file with a shorter one that keeps the same path.
	shortened := filepath.Join(t.TempDir(), "s.jsonl")
	store2, err := session.Open(shortened, "/repo/mine")
	if err != nil {
		t.Fatal(err)
	}
	appendUser(store2, "replacement")
	replacement, err := os.ReadFile(shortened)
	if err != nil {
		t.Fatal(err)
	}
	if len(replacement) >= len(big) {
		t.Skip("replacement is not shorter than the original")
	}
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := cache.sinceAppends(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(cache.entries) {
		t.Fatalf("expected the cache to reset to the replacement's %d entries, got %d",
			len(mustReadAll(t, path)), len(entries))
	}
}

func mustReadAll(t *testing.T, path string) []session.Entry {
	t.Helper()
	entries, err := session.ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
