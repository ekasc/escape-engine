package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Info is the session metadata the desktop shell's reader computes (readSessionInfo).
type Info struct {
	ID            string `json:"id"`
	Path          string `json:"path"`
	Cwd           string `json:"cwd"`
	Name          string `json:"name,omitempty"`
	FirstUserText string `json:"firstUserText,omitempty"`
	StartedAt     string `json:"startedAt,omitempty"`
	Mtime         int64  `json:"mtime"`
	Size          int64  `json:"size"`
	IsWorktree    bool   `json:"isWorktree,omitempty"`
	ParentPath    string `json:"parentPath,omitempty"`
}

const edgeBytes = 96 * 1024

// ReadInfo mirrors the desktop shell's readSessionInfo: reads bounded head+tail windows
// (never O(file)) and extracts id, cwd, name (last non-empty session_info
// name) and the first user message text. Returns nil if the file has no
// session header.
func ReadInfo(path string) (*Info, error) {
	stat, err := os.Stat(path)
	if err != nil || stat.Size() == 0 {
		return nil, err
	}
	size := stat.Size()

	headLen := size
	if headLen > edgeBytes {
		headLen = edgeBytes
	}
	head, err := readRange(path, 0, headLen)
	if err != nil {
		return nil, err
	}
	headObjects := parseLines(head)

	var tailObjects []any
	if size > edgeBytes {
		tailRaw, err := readRange(path, size-edgeBytes, edgeBytes)
		if err != nil {
			return nil, err
		}
		// First tail fragment may begin halfway through a record.
		if i := bytes.IndexByte(tailRaw, '\n'); i >= 0 {
			tailRaw = tailRaw[i+1:]
		}
		tailObjects = parseLines(tailRaw)
	} else {
		tailObjects = headObjects
	}

	header := firstSession(headObjects)
	if header == nil {
		return nil, nil
	}

	info := &Info{
		ID:        header.ID,
		Path:      path,
		Cwd:       header.Cwd,
		StartedAt: header.Timestamp,
		Mtime:     stat.ModTime().UnixMilli(),
		Size:      size,
	}
	if header.ParentSession != "" {
		info.IsWorktree = true
		info.ParentPath = header.ParentSession
	}

	// Name: last session_info with a non-empty name across head+tail.
	for _, obj := range append(append([]any{}, headObjects...), tailObjects...) {
		e, ok := obj.(Entry)
		if !ok || e.Type != TypeSessionInfo || strings.TrimSpace(e.Name) == "" {
			continue
		}
		info.Name = strings.TrimSpace(e.Name)
	}

	// First user text (head only, matching the desktop shell).
	for _, obj := range headObjects {
		e, ok := obj.(Entry)
		if !ok || e.Type != TypeMessage || e.Message == nil || e.Message.Role != RoleUser {
			continue
		}
		text := strings.TrimSpace(e.Message.Text(false))
		if text != "" {
			info.FirstUserText = truncate(text, 110)
			if info.Name == "" {
				info.Name = info.FirstUserText
			}
			break
		}
	}
	return info, nil
}

// Tail reads the last maxBytes of the file aligned to line boundaries,
// mirroring the desktop shell's readSessionTail. Returns entries and the byte offset of
// the first parsed line (for paging older windows).
func Tail(path string, maxBytes int64) ([]Entry, int64, error) {
	return Range(path, nil, maxBytes)
}

// Range mirrors the desktop shell's readSessionRange: a window ending at endOffset
// (nil = EOF), costing O(maxBytes), never O(file size). If the window ends
// mid-record it extends backward until a complete line appears.
func Range(path string, endOffset *int64, maxBytes int64) ([]Entry, int64, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	end := stat.Size()
	if endOffset != nil {
		end = *endOffset
	}
	window := maxBytes
	for {
		start := end - window
		if start < 0 {
			start = 0
		}
		raw, err := readRange(path, start, end-start)
		if err != nil {
			return nil, 0, err
		}
		from := 0
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			from = i + 1
		}
		entries := parseEntries(raw[from:])
		if len(entries) > 0 || start == 0 {
			return entries, start + int64(from), nil
		}
		window *= 2
	}
}

// List returns session infos under root, newest first by mtime.
func List(root string) ([]Info, error) {
	dirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer FlushIndex(root)
	var infos []Info
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(root, d.Name(), f.Name())
			info, err := indexInfo(root, path)
			if err != nil || info == nil {
				continue
			}
			infos = append(infos, *info)
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Mtime > infos[j].Mtime })
	return infos, nil
}

// --- helpers ---

func readRange(path string, start, length int64) ([]byte, error) {
	if length <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, length)
	n, err := f.ReadAt(buf, start)
	if err != nil && n == 0 {
		return nil, err
	}
	return buf[:n], nil
}

func parseLines(raw []byte) []any {
	var objs []any
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		objs = append(objs, e)
	}
	return objs
}

func parseEntries(raw []byte) []Entry {
	var entries []Entry
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries
}

// LastName returns the last non-empty session_info name among entries (the
// session title), mirroring the desktop shell's getSessionName() semantics.
func LastName(entries []Entry) string {
	var name string
	for _, e := range entries {
		if e.Type == TypeSessionInfo && strings.TrimSpace(e.Name) != "" {
			name = strings.TrimSpace(e.Name)
		}
	}
	return name
}

// Name returns the current title of a session file ("" if none has been
// written yet).
func Name(path string) (string, error) {
	entries, err := ReadAll(path)
	if err != nil {
		return "", err
	}
	return LastName(entries), nil
}

func firstSession(objs []any) *Entry {
	for _, obj := range objs {
		e, ok := obj.(Entry)
		if ok && e.Type == TypeSession && e.ID != "" {
			return &e
		}
	}
	return nil
}

func truncate(s string, n int) string {
	oneLine := strings.Join(strings.Fields(s), " ")
	r := []rune(oneLine)
	if len(r) <= n {
		return oneLine
	}
	return string(r[:n-1]) + "…"
}

// Since reads entries appended after from, returning them and the offset to
// resume from. Range walks backwards from an end offset; this walks forwards from
// a start offset, which is the shape an append-only transcript needs.
//
// A session file is opened O_APPEND and nothing truncates it, so the bytes before
// from cannot change and a caller can keep from across turns instead of re-reading
// the whole file. A partial trailing line is left unconsumed rather than parsed,
// so a reader that races a writer resumes cleanly at the next newline.
//
// If the file is shorter than from, it was replaced or truncated, so the read
// restarts from zero and returns the offset it actually reached. Callers that
// treat the returned entries as append-only must handle that case.
func Since(path string, from int64, maxBytes int64) ([]Entry, int64, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	size := stat.Size()
	if from < 0 || from > size {
		from = 0
	}
	limit := size
	if maxBytes > 0 && from+maxBytes < limit {
		limit = from + maxBytes
	}
	raw, err := readRange(path, from, limit-from)
	if err != nil {
		return nil, 0, err
	}
	// Only whole lines are consumed, and the offset stops at the last one. This
	// has to hold at end of file too, not just at a window boundary: a reader
	// that lands mid-entry would otherwise advance past bytes it could not parse
	// and lose that entry for good. The engine writes whole lines under the
	// store lock, so in practice the file always ends on a newline, and this
	// costs one scan of the tail.
	consumed := 0
	if i := bytes.LastIndexByte(raw, '\n'); i >= 0 {
		consumed = i + 1
	}
	return parseEntries(raw[:consumed]), from + int64(consumed), nil
}
