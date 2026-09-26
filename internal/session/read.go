package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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

// Page is a window of transcript entries, bounded in both directions.
type Page struct {
	Entries []Entry `json:"entries"`
	// Leaf is the last entry id, the cursor for reading what comes next.
	Leaf string `json:"leafId"`
	// Earliest is the first entry id, the cursor for reading further back.
	Earliest string `json:"earliestId"`
	// HasMore reports whether anything precedes the window.
	HasMore bool `json:"hasMore"`
	// Total is the number of entries in the file, when it was counted.
	Total int `json:"total,omitempty"`
}

// DefaultPageSize is how much of a transcript is loaded before someone scrolls
// back for more. It is a page of reading, not a budget: a person reading a
// conversation needs the recent turns, not all twelve thousand.
const DefaultPageSize = 200

// PageBefore returns up to limit entries ending at the entry just before
// beforeID, so a caller can walk backwards through a long transcript.
//
// It reads only the file's tail rather than parsing the whole thing, because
// the failure this prevents is a 50MB read and a twelve-thousand node render
// just to show the most recent exchange.
func PageBefore(path, beforeID string, limit int) (Page, error) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	entries, err := readTailWindow(path, beforeID, limit)
	if err != nil {
		return Page{}, err
	}
	content := filterContent(entries)
	total := len(content)

	// The cursor is located in the whole window before anything is truncated,
	// because truncating to the newest limit would put a cursor at the start of
	// the window at index zero and make the page before it look empty.
	cut := -1
	if beforeID != "" {
		cut = indexOfEntry(content, beforeID)
		if cut < 0 {
			// The cursor is further back than the window reached, which the
			// read loop normally prevents. Treating it as "at the start" would
			// silently skip entries, so say the page is unfinished instead.
			return Page{Earliest: content[0].ID, HasMore: true, Total: total}, nil
		}
	} else {
		cut = total
	}

	// Take up to limit entries ending just before the cursor. HasMore is
	// decided by whether the window reached the start of the transcript, not by
	// the page being full: the last page of a session is usually full too.
	start := cut - limit
	if start < 0 {
		start = 0
	}
	page := Page{Entries: content[start:cut], HasMore: start > 0, Total: total}
	if len(page.Entries) > 0 {
		page.Earliest = page.Entries[0].ID
		page.Leaf = page.Entries[len(page.Entries)-1].ID
	}
	return page, nil
}

// readTailWindow reads the last n entries by reading the file's tail in
// chunks, so the cost is proportional to what is returned rather than to the
// size of the transcript.
//
// When a cursor is supplied the window keeps growing backwards until it contains
// that entry, because every page asks for the same tail and a fixed window would
// return the same page forever.
func readTailWindow(path, beforeID string, n int) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	// Read a generous tail: entries are small, and over-reading a little is
	// far cheaper than a second pass. It grows if the window fills up.
	window := int64(256 * 1024)
	const maxWindow = 64 * 1024 * 1024
	var found []Entry
	for window <= maxWindow {
		offset := int64(0)
		if info.Size() > window {
			offset = info.Size() - window
		}
		buf := make([]byte, info.Size()-offset)
		if _, err := f.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if offset > 0 {
			// The first line is probably cut in half by the offset.
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				buf = buf[i+1:]
			}
		}
		found = parseEntries(buf)
		content := filterContent(found)
		covered := int64(len(buf)) >= info.Size()
		if covered {
			return found, nil
		}
		if beforeID == "" {
			// No cursor: the newest n entries are enough once we have more
			// than n of them, or once we have read the whole file.
			if len(content) > n {
				return found, nil
			}
		} else if idx := indexOfEntry(content, beforeID); idx > 0 {
			// The cursor is in this window with something behind it, so the
			// page before it is here. Reaching the cursor exactly at the start
			// of the window is not enough: there would be nothing to return.
			return found, nil
		} else if idx == 0 && covered {
			return found, nil
		}
		window *= 4
	}
	return found, nil
}

func indexOfEntry(entries []Entry, id string) int {
	for i, e := range entries {
		if e.ID == id {
			return i
		}
	}
	return -1
}

func filterContent(entries []Entry) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.Type == TypeSession {
			continue
		}
		out = append(out, e)
	}
	return out
}
