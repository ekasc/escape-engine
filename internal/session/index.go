package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The index exists because session metadata lives at the far end of the file.
// ReadInfo reads a 96KB head and a 96KB tail to recover a title, so listing
// every session means reading most of the transcript. Measured on a real store
// of 383 sessions totalling 669MB:
//
//	stat per session       2ms
//	cheap header probe    14ms
//	ReadInfo per session 514ms
//
// The information is the same in all three cases. This caches it.
//
// The index is never authoritative. Every entry carries the size and mtime it
// was derived from, and an entry is only used when both still match the file on
// disk. Session files are append-only, so any write changes the size, which
// means freshness is decided by the filesystem rather than by bookkeeping that
// could be wrong. A stale, corrupt, or missing index costs one slow pass and
// then rebuilds itself.

const indexVersion = 1

// indexEntry is what ReadInfo would otherwise have to read out of the file.
type indexEntry struct {
	Cwd           string `json:"cwd"`
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	FirstUserText string `json:"firstUserText,omitempty"`
	StartedAt     string `json:"startedAt,omitempty"`
	IsWorktree    bool   `json:"isWorktree,omitempty"`
	ParentPath    string `json:"parentPath,omitempty"`
	Mtime         int64  `json:"mtime"`
	Size          int64  `json:"size"`

	// Stamp is what freshness is judged on, and it is deliberately not Mtime.
	// Info.Mtime is reported in milliseconds because that is the unit the rest
	// of the system and the shell already use; comparing it against a
	// nanosecond stat would never match.
	Stamp int64 `json:"stamp"`
}

type indexDocument struct {
	Version int                   `json:"version"`
	Entries map[string]indexEntry `json:"entries"`
	Mtime   int64                 `json:"mtime"`
}

var (
	indexMu      sync.Mutex
	indexRoot    string
	indexEntries map[string]indexEntry
	indexDirty   bool
)

// indexFile is where the index lives inside a store. It is a file, not a
// directory, so every scan that walks the store skips it.
func indexFile(root string) string { return filepath.Join(root, ".escape-index.json") }

// resetIndexForTest drops the in-process copy so a test sees what is on disk.
func resetIndexForTest() {
	indexMu.Lock()
	indexRoot = ""
	indexEntries = nil
	indexDirty = false
	indexMu.Unlock()
}

// loadIndexLocked makes sure the in-process index is the one for root. It must
// be called with indexMu held.
//
// The index map is never handed out. Returning it would mean every reader held
// a reference the writer knew nothing about, which is a data race rather than
// a lock.
func loadIndexLocked(root string) {
	if indexRoot == root && indexEntries != nil {
		return
	}
	entries := map[string]indexEntry{}
	raw, err := os.ReadFile(indexFile(root))
	if err == nil {
		var doc indexDocument
		if json.Unmarshal(raw, &doc) == nil && doc.Version == indexVersion && doc.Entries != nil {
			entries = doc.Entries
		}
		// A version mismatch, a truncated file, or garbage all land here as an
		// empty index, which is exactly the state a first run is in.
	}
	indexRoot, indexEntries, indexDirty = root, entries, false
}

// indexInfo returns the metadata for path, reading the file only when the
// cached copy no longer matches what is on disk.
func indexInfo(root, path string) (*Info, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if stat.Size() == 0 {
		return nil, nil
	}

	// The lock covers map access only. Reading the file happens unlocked,
	// because holding a mutex across a 192KB read would serialise every
	// concurrent listing behind the slowest one.
	indexMu.Lock()
	loadIndexLocked(root)
	cached, ok := indexEntries[path]
	indexMu.Unlock()

	if ok && cached.Size == stat.Size() && cached.Stamp == stat.ModTime().UnixNano() {
		return &Info{
			ID:            cached.ID,
			Path:          path,
			Cwd:           cached.Cwd,
			Name:          cached.Name,
			FirstUserText: cached.FirstUserText,
			StartedAt:     cached.StartedAt,
			IsWorktree:    cached.IsWorktree,
			ParentPath:    cached.ParentPath,
			Mtime:         cached.Mtime,
			Size:          cached.Size,
		}, nil
	}

	info, err := ReadInfo(path)
	if err != nil || info == nil {
		return nil, err
	}
	indexMu.Lock()
	loadIndexLocked(root)
	indexEntries[path] = indexEntry{
		Cwd:           info.Cwd,
		ID:            info.ID,
		Name:          info.Name,
		FirstUserText: info.FirstUserText,
		StartedAt:     info.StartedAt,
		IsWorktree:    info.IsWorktree,
		ParentPath:    info.ParentPath,
		Mtime:         info.Mtime,
		Size:          info.Size,
		Stamp:         stat.ModTime().UnixNano(),
	}
	indexDirty = true
	indexMu.Unlock()
	return info, nil
}

// FlushIndex writes the index if anything changed. Callers defer this so a pass
// over hundreds of sessions produces one write rather than hundreds.
func FlushIndex(root string) {
	indexMu.Lock()
	if !indexDirty || indexRoot != root {
		indexMu.Unlock()
		return
	}
	// The map is copied rather than referenced. Marshalling it after unlocking
	// would read the same map another listing is writing to, which is a data
	// race; holding the lock across the marshal would serialise every listing
	// behind a multi-megabyte encode.
	snapshot := make(map[string]indexEntry, len(indexEntries))
	for k, v := range indexEntries {
		snapshot[k] = v
	}
	indexDirty = false
	indexMu.Unlock()

	doc := indexDocument{
		Version: indexVersion,
		Entries: snapshot,
		Mtime:   time.Now().UnixNano(),
	}

	raw, err := json.Marshal(doc)
	if err != nil {
		return
	}
	// A unique temp name: a fixed suffix lets two Escape processes race, and
	// the loser fails to rename a file the winner already moved.
	tmp, err := os.CreateTemp(root, ".escape-index-*.tmp")
	if err != nil {
		return
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	// Best effort. A failed index write costs a slow pass, never correctness.
	_ = os.Rename(name, indexFile(root))
}

// RefreshIndex rebuilds the index from the sessions actually on disk and
// returns how many sessions it holds. Entries for files that no longer exist
// are dropped, so this is also the way to compact an index that has gone stale
// after sessions were removed.
func RefreshIndex(root string) (int, error) {
	indexMu.Lock()
	indexEntries = map[string]indexEntry{}
	indexRoot, indexDirty = root, true
	indexMu.Unlock()

	dirs, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		FlushIndex(root)
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	kept := 0
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		files, readErr := os.ReadDir(filepath.Join(root, dir.Name()))
		if readErr != nil {
			continue
		}
		for _, file := range files {
			if file.IsDir() || filepath.Ext(file.Name()) != ".jsonl" {
				continue
			}
			if info, infoErr := indexInfo(root, filepath.Join(root, dir.Name(), file.Name())); infoErr == nil && info != nil {
				kept++
			}
		}
	}
	FlushIndex(root)
	return kept, nil
}

// DropIndex removes the index file and the in-process copy. Used after sessions
// are pruned, so the next pass rebuilds instead of carrying dead entries.
func DropIndex(root string) {
	resetIndexForTest()
	_ = os.Remove(indexFile(root))
}

// IndexPath is the index location, exposed so tooling can report it.
func IndexPath(root string) string { return indexFile(root) }
