package session

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func writeSessionIn(t *testing.T, root, dirCwd, headerCwd, name string) string {
	t.Helper()
	dir := filepath.Join(root, SlugForDir(dirCwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, NewSessionID()+".jsonl")
	store, err := Open(path, headerCwd)
	if err != nil {
		t.Fatal(err)
	}
	if name != "" {
		if _, err := store.Append(Entry{Type: TypeSessionInfo, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// ListForCwd must return exactly what filtering List returns, or the sidebar
// could gain or lose rows depending on which path a caller took.
func TestListForCwdMatchesFilteredList(t *testing.T) {
	root := t.TempDir()
	mine, other := "/repo/mine", "/repo/other"
	writeSessionIn(t, root, mine, mine, "Alpha")
	writeSessionIn(t, root, mine, mine, "Beta")
	writeSessionIn(t, root, other, other, "Gamma")

	want := filepath.Clean(mine)
	all, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	var expected []string
	for _, info := range all {
		if filepath.Clean(info.Cwd) == want {
			expected = append(expected, info.Path)
		}
	}
	sort.Strings(expected)

	got, err := ListForCwd(root, mine)
	if err != nil {
		t.Fatal(err)
	}
	var actual []string
	for _, info := range got {
		actual = append(actual, info.Path)
	}
	sort.Strings(actual)

	if len(actual) != len(expected) {
		t.Fatalf("got %d sessions, filtered List gives %d: %v vs %v", len(actual), len(expected), actual, expected)
	}
	for i := range actual {
		if actual[i] != expected[i] {
			t.Fatalf("row %d differs: %s vs %s", i, actual[i], expected[i])
		}
	}
	if len(actual) != 2 {
		t.Fatalf("expected the two sessions under %s, got %v", mine, actual)
	}
}

// A session's directory comes from an unresolved path while its header records
// the resolved one, so macOS /var and /private/var disagree for the same
// directory. Filtering by directory name would silently drop these, so the
// filter has to read the header.
func TestListForCwdIgnoresTheDirectoryName(t *testing.T) {
	root := t.TempDir()
	unresolved := "/var/folders/tmp/sessiontest"
	resolved := "/private/var/folders/tmp/sessiontest"
	path := writeSessionIn(t, root, unresolved, resolved, "Symlinked")

	if filepath.Clean(unresolved) == filepath.Clean(resolved) {
		t.Skip("this platform does not distinguish the two paths")
	}
	// The directory is named after the unresolved path, so a directory-name
	// filter would miss it entirely.
	if _, err := os.Stat(filepath.Join(root, SlugForDir(resolved))); err == nil {
		t.Fatal("fixture is not exercising the mismatch")
	}

	byHeader, err := ListForCwd(root, resolved)
	if err != nil {
		t.Fatal(err)
	}
	if len(byHeader) != 1 || byHeader[0].Path != path {
		t.Fatalf("header-based filter lost the session: %v", byHeader)
	}
}

func TestListForCwdIsNewestFirst(t *testing.T) {
	root := t.TempDir()
	mine := "/repo/mine"
	older := writeSessionIn(t, root, mine, mine, "Older")
	newer := writeSessionIn(t, root, mine, mine, "Newer")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(newer, future, future); err != nil {
		t.Fatal(err)
	}

	got, err := ListForCwd(root, mine)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
	if got[0].Path != newer || got[1].Path != older {
		t.Fatalf("not newest first: %s then %s", got[0].Path, got[1].Path)
	}
}

func TestListForCwdOnMissingRoot(t *testing.T) {
	got, err := ListForCwd(filepath.Join(t.TempDir(), "nope"), "/repo/mine")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

func TestReadCwdRejectsFilesWithoutAHeader(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadCwd(empty); ok {
		t.Fatal("an empty file has no session header")
	}
	truncated := filepath.Join(dir, "truncated.jsonl")
	if err := os.WriteFile(truncated, []byte(`{"type":"session","id":"a","cwd":"/x"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadCwd(truncated); ok {
		t.Fatal("a truncated header must not parse as a session")
	}
}

func TestListForCwdIgnoresNonSessionFiles(t *testing.T) {
	root := t.TempDir()
	mine := "/repo/mine"
	dir := filepath.Join(root, SlugForDir(mine))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"notes.txt", "README.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := ListForCwd(root, mine)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no sessions, got %v", got)
	}
}

func TestListForCwdSkipsDirectoriesItCannotRead(t *testing.T) {
	root := t.TempDir()
	mine := "/repo/mine"
	writeSessionIn(t, root, mine, mine, "Visible")
	if err := os.MkdirAll(filepath.Join(root, "unreadable"), 0o000); err != nil {
		t.Skipf("cannot make a directory unreadable here: %v", err)
	}
	got, err := ListForCwd(root, mine)
	if err != nil {
		t.Fatalf("an unreadable sibling must not fail the list: %v", err)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0].Name, "") || got[0].Name != "Visible" {
		t.Fatalf("expected the readable session, got %v", got)
	}
}

// The probe is sized against real headers, so a realistic one must parse. A
// header larger than the probe is treated as unreadable, which is why the
// constant carries headroom over the observed maximum.
func TestReadCwdHandlesARealisticHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	header := `{"type":"session","version":3,"id":"0199abcd-0000-7000-8000-000000000000","parentId":"","timestamp":"2026-09-25T12:00:00.000Z","cwd":"/Users/someone/Projects/really-long-directory-name-here","parentSession":"","name":""}`
	if len(header) > headerProbeBytes {
		t.Fatalf("fixture header is %d bytes, larger than the %d byte probe", len(header), headerProbeBytes)
	}
	if err := os.WriteFile(path, []byte(header+"\n"+`{"type":"message","id":"e1","role":"user","content":[]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd, ok := ReadCwd(path)
	if !ok {
		t.Fatal("a realistic header should parse")
	}
	if cwd != "/Users/someone/Projects/really-long-directory-name-here" {
		t.Fatalf("cwd = %q", cwd)
	}
}
