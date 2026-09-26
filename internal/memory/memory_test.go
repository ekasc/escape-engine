package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAddAndRead(t *testing.T) {
	root := t.TempDir()
	store := Open(root, "/repo/escape")

	if got, err := store.Entries(); err != nil || len(got) != 0 {
		t.Fatalf("a missing file should be an empty store, got %v err=%v", got, err)
	}
	if err := store.Add("Session header lives in agent.New"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := store.Add("Provider keys never reach the renderer"); err != nil {
		t.Fatalf("add: %v", err)
	}

	entries, err := store.Entries()
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(entries) != 2 || entries[0] != "Session header lives in agent.New" {
		t.Fatalf("unexpected entries: %#v", entries)
	}
}

func TestAddRejectsWhenFull(t *testing.T) {
	store := Open(t.TempDir(), "/repo/escape")
	if err := store.Add(strings.Repeat("a", MaxChars-10)); err != nil {
		t.Fatalf("first add should fit: %v", err)
	}
	err := store.Add(strings.Repeat("b", 40))
	if err == nil {
		t.Fatal("expected a write past the cap to fail")
	}
	if !strings.Contains(err.Error(), "full") {
		t.Fatalf("error should say the store is full, got %v", err)
	}
	// The rejected write must not have been persisted.
	entries, _ := store.Entries()
	if len(entries) != 1 {
		t.Fatalf("rejected write leaked into the store: %#v", entries)
	}
}

func TestReplaceAndRemove(t *testing.T) {
	store := Open(t.TempDir(), "/repo/escape")
	for _, e := range []string{"one", "two", "three"} {
		if err := store.Add(e); err != nil {
			t.Fatalf("add %q: %v", e, err)
		}
	}
	if err := store.Replace(2, "TWO"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	entries, _ := store.Entries()
	if entries[1] != "TWO" {
		t.Fatalf("replace did not take: %#v", entries)
	}
	if err := store.Remove(1); err != nil {
		t.Fatalf("remove: %v", err)
	}
	entries, _ = store.Entries()
	if len(entries) != 2 || entries[0] != "TWO" || entries[1] != "three" {
		t.Fatalf("remove left the wrong entries: %#v", entries)
	}
}

func TestOutOfRangeIsAnError(t *testing.T) {
	store := Open(t.TempDir(), "/repo/escape")
	if err := store.Replace(1, "x"); err == nil {
		t.Fatal("replacing a missing entry should fail")
	}
	if err := store.Remove(9); err == nil {
		t.Fatal("removing a missing entry should fail")
	}
}

func TestPathIsPerProject(t *testing.T) {
	root := t.TempDir()
	a := Open(root, "/repo/escape")
	b := Open(root, "/repo/shell")
	if a.Path() == b.Path() {
		t.Fatal("different projects must not share a memory file")
	}
	if !strings.HasSuffix(a.Path(), FileName) {
		t.Fatalf("unexpected memory path: %s", a.Path())
	}
}

func TestRenderIsEmptyWhenNoEntries(t *testing.T) {
	store := Open(t.TempDir(), "/repo/escape")
	if got := store.Render(); got != "" {
		t.Fatalf("an empty store should render nothing, got %q", got)
	}
}

func TestRenderShowsUsageAndNumberedEntries(t *testing.T) {
	store := Open(t.TempDir(), "/repo/escape")
	if err := store.Add("alpha"); err != nil {
		t.Fatal(err)
	}
	if err := store.Add("beta"); err != nil {
		t.Fatal(err)
	}
	out := store.Render()
	for _, want := range []string{"MEMORY", "1. alpha", "2. beta", "%", "snapshot"} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q:\n%s", want, out)
		}
	}
}

func TestParseIgnoresHeadingAndJoinsContinuations(t *testing.T) {
	root := t.TempDir()
	path := PathFor(root, "/repo/escape")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := "# Escape memory\n\nDurable notes for this project. Bounded at 2200 characters.\n- first entry\n  continued here\n- second entry\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := Open(root, "/repo/escape").Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %#v", entries)
	}
	if entries[0] != "first entry continued here" {
		t.Fatalf("continuation line not joined: %q", entries[0])
	}
}

func TestMemoryIsSeparatePerProject(t *testing.T) {
	root := t.TempDir()
	escape := Open(root, "/repo/escape")
	shell := Open(root, "/repo/shell")
	if err := escape.Add("escape only"); err != nil {
		t.Fatal(err)
	}
	entries, _ := shell.Entries()
	if len(entries) != 0 {
		t.Fatalf("shell memory should be empty, got %#v", entries)
	}
}

// Concurrent writers must not corrupt the store. A fixed temp-file name lets two
// writers share one path, and the loser fails to rename a file the winner moved.
func TestConcurrentWritesDoNotCorrupt(t *testing.T) {
	store := Open(t.TempDir(), "/repo/escape")
	if err := store.Add("seed"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				errs <- store.Add(fmt.Sprintf("note-%d", n))
			} else {
				_, _ = store.Entries()
				_, _, _ = store.Snapshot()
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent add failed: %v", err)
		}
	}
	entries, err := store.Entries()
	if err != nil {
		t.Fatalf("store unreadable after concurrent writes: %v", err)
	}
	if len(entries) != 21 {
		t.Fatalf("expected 20 notes plus the seed, got %d: %#v", len(entries), entries)
	}
	if entries[0] != "seed" {
		t.Fatalf("concurrent writes reordered the store: %#v", entries)
	}
}

func TestSnapshotIsConsistent(t *testing.T) {
	store := Open(t.TempDir(), "/repo/escape")
	if err := store.Add("alpha"); err != nil {
		t.Fatal(err)
	}
	entries, used, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || used != len("alpha") {
		t.Fatalf("snapshot disagreed with itself: %#v used=%d", entries, used)
	}
}
