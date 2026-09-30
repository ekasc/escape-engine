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
	cwd  string
	// f stays nil until there is something to write. Opening a store is not
	// what makes a session exist — see Open.
	f  *os.File
	mu sync.Mutex

	// needsHeader records that the file is absent or empty, so the first write
	// has to lay down the session header before the entry that triggered it.
	needsHeader bool

	id     string
	lastID string
}

// Open prepares a session file at path without creating it.
//
// It used to create the file and write a `session` header immediately, which
// meant that merely *opening* a store made a session exist. The server opens a
// store at boot and again on every project switch, so each launch and each
// project switch left behind an empty session that nobody had sent anything
// to — they showed up as rows of "Untitled session" in the shell's session
// list, which is a thing that did not happen to anyone.
//
// The file and its header are written together on the first Append, so a
// session exists exactly when something has been sent to it. Until then the
// path is not on disk, and while ID is already reserved — see reserveID.
func Open(path, cwd string) (*Store, error) {
	s := &Store{path: path, cwd: cwd}

	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		s.reserveID()
		return s, nil
	case err != nil:
		return nil, err
	case info.Size() == 0:
		// Present but empty: a header still has to be written before content.
		s.reserveID()
		return s, nil
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s.f = f
	// Existing file: recover id + last entry id from the tail.
	s.id, s.lastID, err = scanHeader(path)
	if err != nil {
		f.Close()
		s.f = nil
		return nil, err
	}
	return s, nil
}

// reserveID assigns the session id up front, without touching the filesystem.
//
// The id has to exist before anything is written because the provider is bound
// to it on the first turn, and a turn is what triggers the write. Generating it
// later would mean the provider saw an empty id for the opening turn and the
// real one from the second turn onwards, which reads as two conversations.
//
// An id in memory is not a session. Nothing lists it, and ListForCwd only ever
// sees files, so this keeps "a session exists once something is sent to it"
// while still handing out a stable id from the first moment.
func (s *Store) reserveID() {
	id := NewSessionID()
	s.id = id
	s.lastID = id
	s.needsHeader = true
}

// ensureOpen creates the file and writes the session header on first use.
// Callers must hold s.mu.
func (s *Store) ensureOpen() error {
	if s.f != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	s.f = f
	if !s.needsHeader {
		return nil
	}
	// The id reserved at open, so the header and the provider agree.
	line, err := json.Marshal(Entry{
		Type:      TypeSession,
		Version:   3,
		ID:        s.id,
		Timestamp: NowISO(),
		Cwd:       s.cwd,
	})
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	s.needsHeader = false
	// A new session changes what a header scan would report, so the shared
	// listing cannot be reused for the next caller.
	InvalidateScan()
	return nil
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

	// This is the moment the session comes into existence.
	if err := s.ensureOpen(); err != nil {
		return e, err
	}

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
	// A new session changes what a header scan would report, so the shared
	// listing cannot be reused for the next caller.
	InvalidateScan()
	s.lastID = e.ID
	return e, nil
}

// Close flushes and closes the file. A store that was never written to has no
// file, and closing it is not an error.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	f := s.f
	s.f = nil
	return f.Close()
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
