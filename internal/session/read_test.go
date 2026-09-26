package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A reader that lands mid-entry must not consume the partial bytes, or the entry
// is lost for good once the offset moves past it.
func TestSinceStopsAtTheLastCompleteLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	store, err := Open(path, "/repo/mine")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(Entry{Type: TypeSessionInfo, Name: "complete"}); err != nil {
		t.Fatal(err)
	}
	header, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// One whole line, then a writer caught between two appends.
	whole := `{"type":"message","id":"done","message":{"role":"user","content":[` +
		`{"type":"text","text":"hi"}]}}` + "\n"
	write := func(body string) {
		if err := os.WriteFile(path, append(append([]byte{}, header...), []byte(body)...), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entryLine := strings.TrimSuffix(whole, "\n")

	write(whole + entryLine[:20])
	entries, offset, err := Since(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The file opens with a session header and a session_info, then the whole
	// message line. The partial tail is not one of them.
	if len(entries) != 3 {
		t.Fatalf("expected 3 complete entries, got %d", len(entries))
	}
	if offset != int64(len(header))+int64(len(whole)) {
		t.Fatalf("offset %d should stop after the last complete line at %d",
			offset, len(header)+len(whole))
	}

	// Completing the line makes it readable, which is the point of holding the
	// offset back rather than skipping the bytes.
	write(whole + entryLine + "\n")
	entries, _, err = Since(path, offset, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "done" {
		t.Fatalf("the completed entry should arrive on the next read, got %+v", entries)
	}
}
