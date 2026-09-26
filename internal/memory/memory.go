// Package memory holds the small, bounded, per-project notes that outlive a
// single session.
//
// The session transcript is narrative: it grows, and auto-compaction summarises
// it, so a detail from three compactions ago may only survive as a summary of a
// summary. Memory is the opposite. It is deliberately tiny, human-editable, and
// never compacted, so the facts an agent needs across days stay addressable.
//
// The store fails loud rather than silent: a write that would exceed MaxChars is
// rejected, and the agent has to consolidate or delete something in the same
// turn. Truncating quietly is how a memory turns into a pile of summaries.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ekasc/escape-engine/internal/session"
)

// MaxChars bounds the entry text. ~800 tokens, which is small enough that the
// agent has to choose what is worth keeping.
const MaxChars = 2200

// FileName is the per-project memory file, stored beside that project's
// sessions under the session root.
const FileName = "MEMORY.md"

// ErrFull is returned when a write would push the store past MaxChars.
var ErrFull = fmt.Errorf("memory is full (%d char limit); remove or shorten an entry first", MaxChars)

// PathFor returns the memory file for a project, derived the same way session
// paths are so memory and sessions share one per-project directory.
func PathFor(root, cwd string) string {
	return filepath.Join(root, session.SlugForDir(cwd), FileName)
}

// Store is a project's memory. It is safe for concurrent use: the agent writes
// through the memory tool while a turn is in flight.
type Store struct {
	path string
	mu   sync.Mutex
}

// Open returns the store for a project. A missing file is not an error; it is
// an empty store that will be created on first write.
func Open(root, cwd string) *Store {
	return &Store{path: PathFor(root, cwd)}
}

// Path is the backing file, for diagnostics and the shell.
func (s *Store) Path() string { return s.path }

// Entries returns the current notes in order.
func (s *Store) Entries() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read()
}

// Add appends a note.
func (s *Store) Add(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("memory entry is empty")
	}
	return s.mutate(func(entries []string) ([]string, error) {
		next := append(append([]string{}, entries...), text)
		if err := checkSize(next); err != nil {
			return nil, err
		}
		return next, nil
	})
}

// Replace overwrites the 1-based entry at index.
func (s *Store) Replace(index int, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("memory entry is empty")
	}
	return s.mutate(func(entries []string) ([]string, error) {
		if index < 1 || index > len(entries) {
			return nil, fmt.Errorf("no memory entry %d (have %d)", index, len(entries))
		}
		next := append([]string{}, entries...)
		next[index-1] = text
		if err := checkSize(next); err != nil {
			return nil, err
		}
		return next, nil
	})
}

// Remove deletes the 1-based entry at index.
func (s *Store) Remove(index int) error {
	return s.mutate(func(entries []string) ([]string, error) {
		if index < 1 || index > len(entries) {
			return nil, fmt.Errorf("no memory entry %d (have %d)", index, len(entries))
		}
		next := append([]string{}, entries[:index-1]...)
		return append(next, entries[index:]...), nil
	})
}

// Used reports how much of the budget the entries occupy.
func (s *Store) Used() (int, error) {
	entries, err := s.Entries()
	if err != nil {
		return 0, err
	}
	return size(entries), nil
}

// Snapshot returns entries and usage from a single read, so a caller rendering
// both can never pair one version of the notes with another's size.
func (s *Store) Snapshot() ([]string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return nil, 0, err
	}
	return entries, size(entries), nil
}

// Render produces the block injected into the system prompt. The usage figure
// is deliberate: it tells the agent how much room it has left, so it can decide
// to consolidate before a write fails.
func (s *Store) Render() string {
	entries, err := s.Entries()
	if err != nil || len(entries) == 0 {
		return ""
	}
	used := size(entries)
	var b strings.Builder
	fmt.Fprintf(&b, "\n\nMEMORY (durable notes for this project) [%d%% — %d/%d chars]", used*100/MaxChars, used, MaxChars)
	for i, entry := range entries {
		fmt.Fprintf(&b, "\n%d. %s", i+1, entry)
	}
	b.WriteString("\n\nThis block is a snapshot taken at session start. Writes take effect next session.")
	return b.String()
}

// mutate applies an edit under the lock and writes only if it succeeds.
func (s *Store) mutate(edit func([]string) ([]string, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return err
	}
	next, err := edit(entries)
	if err != nil {
		return err
	}
	return s.write(next)
}

func (s *Store) read() ([]string, error) {
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parse(string(raw)), nil
}

// write is atomic so a crash mid-write cannot leave a half-written memory. The
// temp name is unique: a fixed suffix lets two concurrent writers share it, and
// the loser fails to rename a file the winner already moved.
func (s *Store) write(entries []string) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(render(entries)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.path)
}

// parse reads "- " items, joining indented continuation lines so an entry can
// span several lines. Anything before the first item is ignored, which keeps a
// hand-written heading from becoming a note.
func parse(raw string) []string {
	var entries []string
	var current []string
	flush := func() {
		if len(current) == 0 {
			return
		}
		entries = append(entries, strings.TrimSpace(strings.Join(current, " ")))
		current = nil
	}
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			flush()
			current = []string{strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))}
			continue
		}
		if len(current) > 0 && trimmed != "" {
			current = append(current, trimmed)
		}
	}
	flush()
	return entries
}

func render(entries []string) string {
	if len(entries) == 0 {
		return "# Escape memory\n"
	}
	var b strings.Builder
	b.WriteString("# Escape memory\n\nDurable notes for this project. Bounded at ")
	fmt.Fprintf(&b, "%d characters.\n", MaxChars)
	for _, entry := range entries {
		b.WriteString("- ")
		b.WriteString(strings.ReplaceAll(entry, "\n", " "))
		b.WriteString("\n")
	}
	return b.String()
}

func size(entries []string) int { return len(strings.Join(entries, "")) }

func checkSize(entries []string) error {
	if n := size(entries); n > MaxChars {
		return ErrFull
	}
	return nil
}
