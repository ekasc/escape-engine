package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// A Project is a directory the person has added to the sidebar, rather than one
// inferred from a session that happens to exist.
//
// Escape used to derive spaces from session cwd, which meant a project only
// appeared once a conversation had been started in it. That is the wrong
// direction: the set of projects you work in is a decision, and it should be
// stated rather than inferred from side effects.
type Project struct {
	Path string `json:"path"`
	// Added is when it was added, used only to keep the file readable.
	Added string `json:"added,omitempty"`
}

type file struct {
	Version  int       `json:"version"`
	Projects []Project `json:"projects"`
}

const version = 1

// Store is the added-project list. It lives under Escape's own root so nothing
// of another tool's is touched, and so deleting it loses nothing but the list.
type Store struct {
	mu   sync.Mutex
	path string
}

// EnvRoot lets a caller, and any test, point the project list somewhere other
// than the real home. Without it a test run writes into the person's actual
// configuration, which is exactly what happened once already.
const EnvRoot = "ESCAPE_HOME"

// NewStore returns the store rooted at dir, normally ~/.escape.
func NewStore(dir string) *Store {
	if override := os.Getenv(EnvRoot); override != "" {
		dir = override
	}
	return &Store{path: filepath.Join(dir, "projects.json")}
}

// Path is where the list is kept.
func (s *Store) Path() string { return s.path }

// List returns the added projects in the order they were added.
//
// A missing file is an empty list, not an error: having added nothing yet is the
// normal state, and it must not read as a failure.
func (s *Store) List() ([]Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || f.Projects == nil {
		// A truncated or hand-edited file costs the list, not the app.
		return nil, nil
	}
	return f.Projects, nil
}

// Add records a directory, and reports whether it was newly added.
//
// The path must be a real directory. Accepting anything else would put an
// unusable entry in the sidebar that fails later, at a point where the cause is
// no longer obvious.
func (s *Store) Add(path, addedAt string) (Project, bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Project{}, false, err
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		return Project{}, false, err
	}
	if !info.IsDir() {
		return Project{}, false, &os.PathError{Op: "add", Path: abs, Err: os.ErrInvalid}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.readLocked()
	if err != nil {
		return Project{}, false, err
	}
	for _, p := range existing {
		if p.Path == abs {
			return p, false, nil
		}
	}
	entry := Project{Path: abs, Added: addedAt}
	existing = append(existing, entry)
	if err := s.writeLocked(existing); err != nil {
		return Project{}, false, err
	}
	return entry, true, nil
}

// Remove drops a project from the list. It says nothing about the directory,
// which is never touched.
func (s *Store) Remove(path string) (bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	abs = filepath.Clean(abs)

	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.readLocked()
	if err != nil {
		return false, err
	}
	kept := make([]Project, 0, len(existing))
	removed := false
	for _, p := range existing {
		if p.Path == abs {
			removed = true
			continue
		}
		kept = append(kept, p)
	}
	if !removed {
		return false, nil
	}
	return true, s.writeLocked(kept)
}

// readLocked must be called with s.mu held.
func (s *Store) readLocked() ([]Project, error) {
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, nil
	}
	return f.Projects, nil
}

// writeLocked is atomic with a unique temp name, so a crash cannot leave half a
// list and two writers cannot rename each other's file away.
func (s *Store) writeLocked(projects []Project) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(file{Version: version, Projects: projects}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".projects-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.path)
}
