package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func dirsUnder(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(root, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func names(got []Completion) []string {
	out := make([]string, 0, len(got))
	for _, c := range got {
		out = append(out, c.Name)
	}
	return out
}

func TestCompleteFiltersByTheFragment(t *testing.T) {
	root := t.TempDir()
	dirsUnder(t, root, "escape", "escape-engine", "escape-shell", "other")
	got, err := CompletePath(filepath.Join(root, "esc"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %v, want the three escape directories", names(got))
	}
	for _, c := range got {
		if !strings.HasPrefix(c.Name, "esc") {
			t.Fatalf("%q does not match the fragment", c.Name)
		}
	}
}

// A trailing separator means "inside this", which is the whole point of
// completing a path in two steps.
func TestTrailingSeparatorListsTheDirectory(t *testing.T) {
	root := t.TempDir()
	dirsUnder(t, root, "projects")
	inner := filepath.Join(root, "projects")
	dirsUnder(t, inner, "alpha", "beta")
	got, err := CompletePath(inner+string(filepath.Separator), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want alpha and beta", names(got))
	}
}

func TestTildeExpandsToHome(t *testing.T) {
	root := t.TempDir()
	dirsUnder(t, root, "Projects")
	got, err := CompletePath("~/Pro", root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != filepath.Join(root, "Projects") {
		t.Fatalf("got %+v, want %q under the home directory", names(got), filepath.Join(root, "Projects"))
	}
}

func TestEmptyInputOffersHome(t *testing.T) {
	root := t.TempDir()
	got, err := CompletePath("   ", root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != root {
		t.Fatalf("got %+v, want the home directory", got)
	}
}

// A path that is not a directory offers nothing, rather than an error the field
// would have to explain.
func TestUnreadableOrNonDirectoryPathOffersNothing(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{file + "/x", filepath.Join(root, "missing", "x"), "/nope/nope/nope"} {
		got, err := CompletePath(in, root)
		if err != nil {
			t.Fatalf("%q should not error: %v", in, err)
		}
		if len(got) != 0 {
			t.Fatalf("%q returned %v, want nothing", in, names(got))
		}
	}
}

// Only directories: a project is a directory, and offering files fills the list
// with things that cannot be added.
func TestOnlyDirectoriesAreOffered(t *testing.T) {
	root := t.TempDir()
	dirsUnder(t, root, "thing")
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := CompletePath(root+string(filepath.Separator), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "thing" {
		t.Fatalf("got %v, want only the directory", names(got))
	}
}

// Directories come before hidden ones, because they are the likely target.
func TestHiddenEntriesAreSkippedUntilAskedFor(t *testing.T) {
	root := t.TempDir()
	dirsUnder(t, root, ".hidden", "visible")
	got, _ := CompletePath(root+string(filepath.Separator), "")
	if len(got) != 1 || got[0].Name != "visible" {
		t.Fatalf("got %v, want only visible", names(got))
	}
	// Asking by name reaches it.
	got, _ = CompletePath(filepath.Join(root, ".hi"), "")
	if len(got) != 1 || got[0].Name != ".hidden" {
		t.Fatalf("got %v, want the hidden directory once named", names(got))
	}
}

func TestCompletionKnowsWhetherADirectoryHasAnything(t *testing.T) {
	root := t.TempDir()
	dirsUnder(t, root, "full", "empty")
	// The name only helps if something is actually in it.
	dirsUnder(t, filepath.Join(root, "full"), "something")
	got, _ := CompletePath(root+string(filepath.Separator), "")
	byName := map[string]Completion{}
	for _, c := range got {
		byName[c.Name] = c
	}
	if !byName["full"].HasChildren {
		t.Error("a directory with entries should report having children")
	}
	if byName["empty"].HasChildren {
		t.Error("an empty directory should not report children")
	}
}

func TestCompletionIsCapped(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < MaxCompletions+50; i++ {
		if err := os.MkdirAll(filepath.Join(root, pad(i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := CompletePath(root+string(filepath.Separator), "")
	if len(got) != MaxCompletions {
		t.Fatalf("got %d completions, want the cap of %d", len(got), MaxCompletions)
	}
}

func pad(i int) string {
	return string(rune('a'+i/676)) + string(rune('a'+(i/26)%26)) + string(rune('a'+i%26))
}

// A shared prefix is what makes a second Tab useful rather than a no-op.
func TestCommonPrefixNarrowsInsteadOfRepeating(t *testing.T) {
	if got := CommonPrefix([]string{"/a/abc", "/a/abd"}); got != "/a/ab" {
		t.Fatalf("got %q, want %q", got, "/a/ab")
	}
	if got := CommonPrefix([]string{"/a/abc"}); got != "/a/abc" {
		t.Fatalf("a single candidate should be returned whole, got %q", got)
	}
	// Two unrelated paths share only the separator. That is the honest answer,
	// and it is why the caller compares the result against what is already typed
	// rather than treating any non-empty result as progress.
	if got := CommonPrefix([]string{"/a/x", "/b/y"}); got != "/" {
		t.Fatalf("got %q, want the shared separator", got)
	}
	if got := CommonPrefix(nil); got != "" {
		t.Fatalf("no candidates should be empty, got %q", got)
	}
}

// Paths must be returned sorted with directories first, so the first candidate
// is the likely one rather than alphabetical luck.
func TestCompletionsAreOrderedDirectoriesFirst(t *testing.T) {
	root := t.TempDir()
	dirsUnder(t, root, "bbb")
	if err := os.MkdirAll(filepath.Join(root, "aaa"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, _ := CompletePath(root+string(filepath.Separator), "")
	if len(got) < 2 || !got[0].Dir {
		t.Fatalf("expected directories first, got %+v", got)
	}
}
