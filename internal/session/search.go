package session

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SearchSampleBytes bounds how much of each session is scanned. A session can be
// large, so this reads a head and a tail window rather than the whole file; a
// topic buried in the exact middle of a very long session can be missed, which
// is a deliberate trade for never reading O(file) per candidate.
const SearchSampleBytes = 64 * 1024

// SearchMatch is a session that matched, with the text that matched so the
// caller can show why rather than making the model guess.
type SearchMatch struct {
	Info
	MatchedIn string // "name", "first message", or "transcript"
	Snippet   string
}

// Search finds this project's sessions whose name, first user message, or
// transcript sample contains query, newest first. An empty query returns the
// most recently touched sessions, which is the "what was I on" case.
//
// It is scoped to cwd on purpose: resuming means taking over the engine's single
// agent, so a match the agent cannot act on would be worse than no match.
func Search(root, cwd, query string, limit int) ([]SearchMatch, error) {
	if limit <= 0 {
		limit = 10
	}
	infos, err := List(root)
	if err != nil {
		return nil, err
	}
	want := filepath.Clean(cwd)
	needle := strings.ToLower(strings.TrimSpace(query))

	matches := make([]SearchMatch, 0, limit)
	for _, info := range infos {
		if filepath.Clean(info.Cwd) != want {
			continue
		}
		if needle == "" {
			matches = append(matches, SearchMatch{Info: info, MatchedIn: "recent"})
			if len(matches) == limit {
				break
			}
			continue
		}
		if where, snippet, ok := matchInfo(info, needle); ok {
			matches = append(matches, SearchMatch{Info: info, MatchedIn: where, Snippet: snippet})
			continue
		}
		if snippet, ok := matchTranscript(info.Path, needle); ok {
			matches = append(matches, SearchMatch{Info: info, MatchedIn: "transcript", Snippet: snippet})
		}
	}
	// List already sorts by mtime descending, so the result is newest first.
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Mtime > matches[j].Mtime })
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, nil
}

func matchInfo(info Info, needle string) (string, string, bool) {
	if strings.Contains(strings.ToLower(info.Name), needle) {
		return "name", info.Name, true
	}
	if snippet, ok := snippetAround(info.FirstUserText, needle); ok {
		return "first message", snippet, true
	}
	return "", "", false
}

// matchTranscript scans a bounded head+tail window of the raw file. The JSONL
// is not parsed here: a substring hit is enough to rank a candidate, and
// parsing every entry of every session would make search O(everything).
func matchTranscript(path, needle string) (string, bool) {
	stat, err := os.Stat(path)
	if err != nil || stat.Size() == 0 {
		return "", false
	}
	size := stat.Size()
	headLen := size
	if headLen > SearchSampleBytes {
		headLen = SearchSampleBytes
	}
	head, err := readRange(path, 0, headLen)
	if err != nil {
		return "", false
	}
	if s, ok := snippetAround(string(head), needle); ok {
		return s, true
	}
	if size > SearchSampleBytes {
		tailStart := size - SearchSampleBytes
		tail, err := readRange(path, tailStart, SearchSampleBytes)
		if err == nil {
			if s, ok := snippetAround(string(tail), needle); ok {
				return s, true
			}
		}
	}
	return "", false
}

// snippetAround returns a short window around the first hit, with newlines
// flattened so it stays on one line in the tool output.
func snippetAround(text, needle string) (string, bool) {
	lower := strings.ToLower(text)
	i := strings.Index(lower, needle)
	if i < 0 {
		return "", false
	}
	flat := strings.Join(strings.Fields(text), " ")
	// The flattened offset no longer matches, so search the flattened string.
	flatLower := strings.ToLower(flat)
	j := strings.Index(flatLower, needle)
	if j < 0 {
		return "", false
	}
	start := j - 40
	if start < 0 {
		start = 0
	}
	end := j + len(needle) + 60
	if end > len(flat) {
		end = len(flat)
	}
	out := flat[start:end]
	if start > 0 {
		out = "..." + out
	}
	if end < len(flat) {
		out += "..."
	}
	return out, true
}
