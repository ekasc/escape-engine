package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tree(t *testing.T, dirs ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// Typing a fragment should find the folder, which is the whole point.
func TestFuzzyMatchFindsSubsequences(t *testing.T) {
	cases := []struct {
		query, candidate string
		want             bool
	}{
		{"eng", "Projects/escape/engine", true},
		{"eseng", "Projects/escape/engine", true}, // across a separator
		{"engine", "engine", true},
		{"xyz", "engine", false},
		{"e-g", "engine", false},   // a subsequence cannot skip a character
		{"nin", "engine", true},    // scattered but in order
		{"zzz", "engine", false},   // not present at all
		{"ENGINE", "engine", true}, // case does not matter
		{"", "anything", true},     // an empty query matches everything
	}
	for _, c := range cases {
		if _, ok := FuzzyMatch(c.query, c.candidate); ok != c.want {
			t.Errorf("FuzzyMatch(%q, %q) matched=%v, want %v", c.query, c.candidate, ok, c.want)
		}
	}
}

// Two things match; the one whose name you actually typed should come first.
func TestFuzzyMatchRanksTheFolderNameHighest(t *testing.T) {
	nameMatch, _ := FuzzyMatch("eng", "Projects/engine")
	parentMatch, _ := FuzzyMatch("eng", "engineering/notes")
	if nameMatch <= parentMatch {
		t.Fatalf("matching the folder name (%d) should beat matching a parent (%d)", nameMatch, parentMatch)
	}
	prefix, _ := FuzzyMatch("eng", "engine")
	scattered, _ := FuzzyMatch("eng", "enumerate-generated")
	if prefix <= scattered {
		t.Fatalf("a prefix match (%d) should beat a scattered one (%d)", prefix, scattered)
	}
	consecutive, _ := FuzzyMatch("ine", "engine")
	spread, _ := FuzzyMatch("ine", "indigo-nebula-expanse")
	if consecutive <= spread {
		t.Fatalf("consecutive characters (%d) should beat scattered ones (%d)", consecutive, spread)
	}
}

func TestSearchDirsRanksMatches(t *testing.T) {
	root := tree(t,
		"Projects/escape/engine",
		"Projects/escape/shell",
		"Projects/engineering-notes",
		"Study/algorithms",
	)
	got, err := SearchDirs(root, "eng", 3, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 {
		t.Fatalf("expected matches, got %v", got)
	}
	// The exact folder name beats a longer path that merely contains the letters.
	first := got[0].Name
	if first != filepath.Join("Projects", "engineering-notes") && first != filepath.Join("Projects", "escape", "engine") {
		t.Fatalf("unexpected first result %q in %v", first, got)
	}
	// Both should be present regardless of order.
	// Names are the path relative to the search root, so a row does not repeat
	// itself.
	names := map[string]bool{}
	for _, c := range got {
		names[c.Name] = true
	}
	if !names[filepath.Join("Projects", "escape", "engine")] ||
		!names[filepath.Join("Projects", "engineering-notes")] {
		t.Fatalf("both should match, got %v", names)
	}
}

// An empty query is the common case: open the picker and see somewhere useful.
func TestSearchDirsWithNoQueryListsDirectories(t *testing.T) {
	root := tree(t, "Projects/escape", "Study/algorithms")
	got, err := SearchDirs(root, "", 3, 50)
	if err != nil {
		t.Fatal(err)
	}
	// A walk lists intermediate directories too, which is what makes "Projects"
	// reachable without typing a slash.
	names := map[string]bool{}
	for _, c := range got {
		names[c.Name] = true
	}
	for _, want := range []string{"Projects", filepath.Join("Projects", "escape"), "Study", filepath.Join("Study", "algorithms")} {
		if !names[want] {
			t.Fatalf("%q missing from %v", want, names)
		}
	}
}

// The walk must not descend into the places that make a search slow and useless.
func TestSearchDirsSkipsBuildOutput(t *testing.T) {
	root := tree(t,
		"Projects/escape",
		"Projects/escape/node_modules/left-pad",
		"Projects/escape/node_modules/left-pad/test",
		"Projects/escape/.git/objects",
		"Projects/escape/build/output",
	)
	got, err := SearchDirs(root, "", 4, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		for _, skipped := range []string{"node_modules", ".git", "build"} {
			if c.Path == root || c.Path == filepath.Join(root, "Projects") || c.Path == filepath.Join(root, "Projects", "escape") {
				continue
			}
			if filepath.Base(c.Path) == skipped {
				t.Fatalf("walked into %s: %v", skipped, c.Path)
			}
		}
	}
	if len(got) == 0 {
		t.Fatal("the real project should still be listed")
	}
}

func TestSearchDirsRespectsDepth(t *testing.T) {
	root := tree(t, "a/b/c/d/e")
	shallow, err := SearchDirs(root, "", 2, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range shallow {
		rel, _ := filepath.Rel(root, c.Path)
		if depthOf(rel) > 2 {
			t.Fatalf("%s is deeper than the limit", rel)
		}
	}
	deep, err := SearchDirs(root, "", 5, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(deep) <= len(shallow) {
		t.Fatalf("a deeper walk should find more: %d vs %d", len(deep), len(shallow))
	}
}

func TestSearchDirsOnAMissingRoot(t *testing.T) {
	got, err := SearchDirs(filepath.Join(t.TempDir(), "nope"), "", 3, 10)
	if err != nil {
		t.Fatalf("a missing root should not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want nothing", got)
	}
}

// With nothing typed there is nothing to disambiguate, so the list is what is
// there, in order. Ranking by recency answers a question nobody asked.
func TestEmptyQueryIsAlphabetical(t *testing.T) {
	root := t.TempDir()
	// Written in an order that is neither alphabetical nor by recency.
	for _, d := range []string{"zebra", "alpha", "middle"} {
		mustMkdir(t, filepath.Join(root, d))
	}
	// alpha was touched last, so a recency rule would put it first.
	recent := time.Now()
	if err := os.Chtimes(filepath.Join(root, "alpha"), recent, recent); err != nil {
		t.Fatal(err)
	}
	got, err := SearchDirs(root, "", 3, 50)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	want := []string{"alpha", "middle", "zebra"}
	for i := range want {
		if i >= len(names) || names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
}

// Typing switches ranking on, because then there is a question to answer.
func TestTypedQueryRanksByMatchNotAlphabet(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "aaa-engineering-notes"))
	mustMkdir(t, filepath.Join(root, "zzz-engine"))
	got, err := SearchDirs(root, "engine", 3, 50)
	if err != nil {
		t.Fatal(err)
	}
	// Alphabetically aaa- comes first, but the folder named engine is the
	// better answer.
	if len(got) == 0 || got[0].Name != "zzz-engine" {
		t.Fatalf("got %v, want zzz-engine first", names(got))
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

// Hidden directories are never somewhere to open as a project, and they sort
// ahead of everything real because "." precedes a letter.
func TestHiddenDirectoriesAreSkippedAtEveryDepth(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{
		"alpha",
		".stfolder",
		".vscode",
		".ipynb_checkpoints",
		"beta/.hidden-nested",
		"beta/visible",
	} {
		mustMkdir(t, filepath.Join(root, d))
	}
	got, err := SearchDirs(root, "", 3, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if strings.HasPrefix(filepath.Base(c.Name), ".") {
			t.Fatalf("hidden directory listed: %q", c.Name)
		}
		if strings.Contains(c.Name, ".hidden-nested") {
			t.Fatalf("hidden directory reached: %q", c.Name)
		}
	}
	// The visible ones are all still there, in alphabetical order.
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	if len(names) < 2 || names[0] != "alpha" {
		t.Fatalf("got %v, want alpha first", names)
	}
}
