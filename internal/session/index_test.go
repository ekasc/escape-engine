package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func writeSession(t *testing.T, root, slug, name, text string) string {
	t.Helper()
	path := filepath.Join(root, slug, "2026-01-01T00-00-00-000Z_test.jsonl")
	store, err := Open(path, "/repo/"+slug)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Append(Entry{Type: TypeMessage, Message: &Message{Role: "user", Content: []Block{{Type: BlockText, Text: text}}}}); err != nil {
		t.Fatal(err)
	}
	if name != "" {
		if _, err := store.Append(Entry{Type: TypeSessionInfo, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// The index must never be able to report a title the file does not have. The
// whole safety argument is that a write changes the size, so this is the test
// that would catch a regression in that reasoning.
func TestIndexIsInvalidatedByAnyWrite(t *testing.T) {
	root := t.TempDir()
	resetIndexForTest()
	path := writeSession(t, root, "proj", "Original title", "hello")

	first, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Name != "Original title" {
		t.Fatalf("expected the original title, got %+v", first)
	}
	if _, err := os.Stat(IndexPath(root)); err != nil {
		t.Fatalf("the index should have been written: %v", err)
	}

	// A rename appends a session_info entry, which grows the file.
	store, err := Open(path, "/repo/proj")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(Entry{Type: TypeSessionInfo, Name: "Renamed"}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	second, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].Name != "Renamed" {
		t.Fatalf("a rename must not be served from a stale index, got %+v", second)
	}
}

// A missing, truncated, or unrecognised index must cost a rebuild, never an
// error and never wrong data.
func TestIndexRecoversFromAMissingOrCorruptFile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(t *testing.T, path string)
	}{
		{"missing", nil},
		{"empty", func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"truncated json", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(`{"version":1,"entr`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong version", func(t *testing.T, path string) {
			raw, _ := json.Marshal(indexDocument{Version: 99, Entries: map[string]indexEntry{
				"/nope": {Name: "lies", Size: 1, Mtime: 1},
			}})
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			resetIndexForTest()
			writeSession(t, root, "proj", "Real title", "hello")

			want, err := List(root)
			if err != nil || len(want) != 1 {
				t.Fatalf("baseline failed: %v %+v", err, want)
			}

			resetIndexForTest()
			if tc.break_ != nil {
				tc.break_(t, IndexPath(root))
			}

			got, err := List(root)
			if err != nil {
				t.Fatalf("a broken index must not fail the listing: %v", err)
			}
			if len(got) != 1 || got[0].Name != "Real title" {
				t.Fatalf("a broken index must not change the answer, got %+v", got)
			}
		})
	}
}

// The index is derived, so deleting it must be harmless.
func TestIndexCanBeDeletedWithNoConsequence(t *testing.T) {
	root := t.TempDir()
	resetIndexForTest()
	writeSession(t, root, "proj", "Title", "hello")
	if _, err := List(root); err != nil {
		t.Fatal(err)
	}
	resetIndexForTest()
	if err := os.Remove(IndexPath(root)); err != nil {
		t.Fatal(err)
	}
	got, err := List(root)
	if err != nil || len(got) != 1 || got[0].Name != "Title" {
		t.Fatalf("listing must survive a deleted index, got %+v err %v", got, err)
	}
}

// The index must not disturb the scans that walk the store, because it is a
// file at the store root rather than a project directory.
func TestIndexIsNotMistakenForAProject(t *testing.T) {
	root := t.TempDir()
	resetIndexForTest()
	writeSession(t, root, "proj", "Title", "hello")
	if _, err := List(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(IndexPath(root)); err != nil {
		t.Fatalf("the index should exist: %v", err)
	}
	found, err := RefreshIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if found != 1 {
		t.Fatalf("the index file must not be counted as a session, got %d", found)
	}
	res, err := PruneEmptyProjectDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Empty) != 0 {
		t.Fatalf("the index must never look like a prunable empty project: %v", res.Empty)
	}
}

// A second listing must be served from the index, not from the files. Truncating
// the session file after the index was built proves which one answered.
func TestSecondListingIsServedFromTheIndex(t *testing.T) {
	root := t.TempDir()
	resetIndexForTest()
	path := writeSession(t, root, "proj", "Cached title", "hello")
	if _, err := List(root); err != nil {
		t.Fatal(err)
	}
	// Make the file unreadable. An index hit still answers; a file read cannot.
	if err := os.Chmod(path, 0o000); err != nil {
		t.Skipf("cannot make unreadable: %v", err)
	}
	defer os.Chmod(path, 0o600)

	resetIndexForTest()
	got, err := List(root)
	if err != nil {
		t.Fatalf("listing failed: %v", err)
	}
	if len(got) != 1 || got[0].Name != "Cached title" {
		t.Fatalf("expected the index to answer, got %+v", got)
	}
}

// Concurrency: two writers must not lose the index or corrupt it, which is what
// a fixed temp file name would cause.
func TestIndexSurvivesConcurrentListings(t *testing.T) {
	root := t.TempDir()
	resetIndexForTest()
	for i := 0; i < 12; i++ {
		writeSession(t, root, fmt.Sprintf("proj-%d", i%6), "t", "hello")
	}
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, err := List(root)
			done <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent listing failed: %v", err)
		}
	}
	resetIndexForTest()
	if _, err := List(root); err != nil {
		t.Fatalf("index was left unreadable: %v", err)
	}
	if _, err := os.Stat(IndexPath(root)); err != nil {
		t.Fatalf("index missing after concurrent writes: %v", err)
	}
}
