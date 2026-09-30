package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPruneOnlyFindsGenuinelyEmptyDirectories(t *testing.T) {
	root := t.TempDir()
	// empty: no files at all
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	// has a session. One entry, because a store no longer creates its file on
	// open — that is the whole point of the change, and a directory holding only
	// an unopened store is genuinely empty and should be pruned.
	dir := filepath.Join(root, "withsession")
	store, err := Open(filepath.Join(dir, "s.jsonl"), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(Entry{Type: TypeMessage, Message: &Message{Role: "user", Content: []Block{{Type: "text", Text: "hi"}}}}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	// has some other file: not empty, must survive
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "notes.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := PruneEmptyProjectDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Empty) != 1 || res.Empty[0] != "empty" {
		t.Fatalf("only the truly empty directory should be reported, got %v", res.Empty)
	}

	removed, err := PruneEmptyProjectDirsApply(root)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 removal, got %d", removed)
	}
	for _, keep := range []string{"withsession", "other"} {
		if _, err := os.Stat(filepath.Join(root, keep)); err != nil {
			t.Fatalf("%s must survive: %v", keep, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "empty")); !os.IsNotExist(err) {
		t.Fatal("the empty directory should be gone")
	}
}

// A directory that gains a file between the scan and the removal is not empty
// any more, and taking it would delete a session.
func TestPruneRefusesADirectoryThatFilledUp(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "race")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// scan says empty
	found, err := PruneEmptyProjectDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Empty) != 1 {
		t.Fatalf("expected the directory to be reported, got %v", found.Empty)
	}
	// a session lands before the removal
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := PruneEmptyProjectDirsApply(root)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("a directory that received a session must not be removed, removed=%d", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "s.jsonl")); err != nil {
		t.Fatalf("the session file must survive: %v", err)
	}
}
