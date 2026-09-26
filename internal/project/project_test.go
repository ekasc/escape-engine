package project

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestListIsEmptyBeforeAnythingIsAdded(t *testing.T) {
	t.Setenv(EnvRoot, t.TempDir())
	s := NewStore(t.TempDir())
	got, err := s.List()
	if err != nil {
		t.Fatalf("a missing list must not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected nothing, got %v", got)
	}
}

func TestAddKeepsInsertionOrderAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	for _, name := range []string{"a", "b", "c"} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Add(p, "now"); err != nil {
			t.Fatal(err)
		}
	}
	// Re-adding an existing project must not duplicate or reorder it.
	again, isNew, err := s.Add(filepath.Join(dir, "b"), "now")
	if err != nil {
		t.Fatal(err)
	}
	if isNew {
		t.Fatal("re-adding an existing project should report it as known")
	}
	if again.Path != filepath.Join(dir, "b") {
		t.Fatalf("re-add returned %q", again.Path)
	}

	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %d projects, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if filepath.Base(got[i].Path) != w {
			t.Fatalf("position %d = %q, want %q", i, filepath.Base(got[i].Path), w)
		}
	}
}

// A path that is not a directory would become a sidebar row that fails later,
// where the cause is no longer obvious.
func TestAddRefusesAnythingButADirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(dir)
	if _, _, err := s.Add(file, "now"); err == nil {
		t.Fatal("adding a file should be refused")
	}
	if _, _, err := s.Add(filepath.Join(dir, "missing"), "now"); err == nil {
		t.Fatal("adding a missing path should be refused")
	}
	got, _ := s.List()
	if len(got) != 0 {
		t.Fatalf("a refused add must not be recorded: %v", got)
	}
}

// The same directory reached by different routes is one project.
func TestAddNormalisesThePath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "project")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	s := NewStore(dir)
	if _, _, err := s.Add(target, "now"); err != nil {
		t.Fatal(err)
	}
	// Trailing separator and a relative hop must resolve to the same entry.
	odd := filepath.Join(dir, "project", "..", "project") + string(filepath.Separator)
	if _, isNew, err := s.Add(odd, "now"); err != nil {
		t.Fatal(err)
	} else if isNew {
		t.Fatal("a differently-spelled path to the same directory is a new project")
	}
	got, _ := s.List()
	if len(got) != 1 {
		t.Fatalf("got %d projects, want 1: %v", len(got), got)
	}
}

func TestRemoveLeavesTheDirectoryAlone(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "project")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	s := NewStore(dir)
	if _, _, err := s.Add(target, "now"); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Remove(target)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("expected the project to be removed from the list")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("removing a project must not touch its directory: %v", err)
	}
	if removed, err := s.Remove(target); err != nil || removed {
		t.Fatalf("removing an absent project should be a no-op, got %v %v", removed, err)
	}
}

// A hand-edited or truncated file costs the list, not the app.
func TestListSurvivesACorruptFile(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := os.WriteFile(s.Path(), []byte(`{"version":1,"projects":[{"path"`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.List()
	if err != nil {
		t.Fatalf("a corrupt list must not fail the read: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected an empty list, got %v", got)
	}
	// And the store must still be usable afterwards.
	p := filepath.Join(dir, "next")
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Add(p, "now"); err != nil {
		t.Fatalf("a corrupt list must not block adding: %v", err)
	}
}

func TestListKeepsProjectPathsPrivate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "project")
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	s := NewStore(dir)
	if _, _, err := s.Add(p, "now"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("projects.json mode = %o, want 600", perm)
	}
}

// The list is written on every add and remove, so concurrent calls must not
// corrupt it or lose an entry.
func TestConcurrentAddsKeepEveryProject(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, 12)
	for i := range paths {
		paths[i] = filepath.Join(dir, string(rune('a'+i)))
		if err := os.MkdirAll(paths[i], 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s := NewStore(dir)
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			_, _, _ = s.Add(p, "now")
		}(p)
	}
	wg.Wait()
	got, err := s.List()
	if err != nil {
		t.Fatalf("the list was left unreadable: %v", err)
	}
	if len(got) != len(paths) {
		t.Fatalf("got %d projects, want %d", len(got), len(paths))
	}
}

// The store must be redirectable, or a test run writes into the real home.
func TestEnvRootRedirectsTheStore(t *testing.T) {
	elsewhere := t.TempDir()
	t.Setenv(EnvRoot, elsewhere)
	s := NewStore(t.TempDir())
	dir := filepath.Join(elsewhere, "project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Add(dir, "now"); err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(s.Path()) != elsewhere {
		t.Fatalf("store path = %q, want it under %q", s.Path(), elsewhere)
	}
}

// A blank path must not resolve to the process directory, which would silently
// add whatever the engine happens to be running in.
func TestAddRefusesABlankPath(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if _, _, err := s.Add("   ", "now"); err == nil {
		t.Fatal("a blank path should be refused")
	}
	got, _ := s.List()
	if len(got) != 0 {
		t.Fatalf("a refused blank path must not be recorded: %v", got)
	}
}
