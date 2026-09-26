package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// ErrEmpty is returned by Store.ID when the file has no session header.
var ErrEmpty = errors.New("session file has no header")

// Store is an append-only writer for one session file. Writes are full-line
// appends so a reader (the desktop shell) never sees a torn line in the middle.
type Store struct {
	path string
	f    *os.File
	mu   sync.Mutex

	id     string
	lastID string
}

// Open opens (creating if needed) a session file at path. If the file is
// empty, a `session` header entry is written first with the given cwd.
func Open(path, cwd string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, f: f}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.Size() == 0 {
		id := NewSessionID()
		header := Entry{
			Type:      TypeSession,
			Version:   3,
			ID:        id,
			Timestamp: NowISO(),
			Cwd:       cwd,
		}
		if _, err := s.Append(header); err != nil {
			f.Close()
			return nil, err
		}
		s.id = id
		s.lastID = id
		return s, nil
	}

	// Existing file: recover id + last entry id from the tail.
	s.id, s.lastID, err = scanHeader(path)
	if err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

// Path returns the session file path.
func (s *Store) Path() string { return s.path }

// ID returns the session id from the header.
func (s *Store) ID() string { return s.id }

// LastID returns the id of the last written entry ("" if none), used as the
// parentId for the next entry.
func (s *Store) LastID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastID
}

// Append persists one entry as a JSONL line. Missing ids/timestamps are
// filled in. It returns the entry as written.
func (s *Store) Append(e Entry) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if e.ID == "" {
		e.ID = NewShortID()
	}
	if e.Timestamp == "" {
		e.Timestamp = NowISO()
	}
	if e.ParentID == "" {
		e.ParentID = s.lastID
	}

	line, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	line = append(line, '\n')
	if _, err := s.f.Write(line); err != nil {
		return e, err
	}
	s.lastID = e.ID
	return e, nil
}

// Close flushes and closes the file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}

// scanHeader reads the whole file (sessions are small in v1) to recover the
// session id and the last entry id.
func scanHeader(path string) (id, lastID string, err error) {
	entries, err := ReadAll(path)
	if err != nil {
		return "", "", err
	}
	for _, e := range entries {
		if e.Type == TypeSession && e.ID != "" {
			id = e.ID
		}
		if e.ID != "" {
			lastID = e.ID
		}
	}
	if id == "" {
		return "", "", ErrEmpty
	}
	return id, lastID, nil
}

// ReadAll parses every line of a session file into entries. Invalid lines are
// skipped (a concurrent tail write may leave a partial line).
func ReadAll(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

var _ io.Closer = (*Store)(nil)
