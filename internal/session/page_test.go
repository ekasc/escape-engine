package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func transcriptOf(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	store, err := Open(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 0; i < n; i++ {
		role := RoleUser
		if i%2 == 1 {
			role = RoleAssistant
		}
		if _, err := store.Append(Entry{Type: TypeMessage, Message: &Message{
			Role: role, Content: []Block{{Type: BlockText, Text: fmt.Sprintf("message number %d", i)}},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func idsOf(p Page) []string {
	out := make([]string, 0, len(p.Entries))
	for _, e := range p.Entries {
		out = append(out, e.ID)
	}
	return out
}

// The first page must be the most recent entries, not the first ones. Loading
// the start of a long session shows a conversation the person finished days ago.
func TestFirstPageIsTheMostRecentEntries(t *testing.T) {
	path := transcriptOf(t, 500)
	page, err := PageBefore(path, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 50 {
		t.Fatalf("got %d entries, want 50", len(page.Entries))
	}
	first := page.Entries[0]
	last := page.Entries[len(page.Entries)-1]
	if first.Message.Content[0].Text != "message number 450" {
		t.Fatalf("first entry = %q, want the 450th message", first.Message.Content[0].Text)
	}
	if last.Message.Content[0].Text != "message number 499" {
		t.Fatalf("last entry = %q, want the final message", last.Message.Content[0].Text)
	}
	if !page.HasMore {
		t.Fatal("a session with 500 entries must report more behind a 50-entry page")
	}
}

// Walking back must eventually cover the whole transcript with no gaps and no
// repeats, or scrollback shows holes.
func TestPagingBackCoversEverythingExactlyOnce(t *testing.T) {
	path := transcriptOf(t, 500)
	seen := map[string]int{}
	order := []string{}
	cursor := ""
	for pages := 0; pages < 50; pages++ {
		page, err := PageBefore(path, cursor, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Entries) == 0 {
			break
		}
		for _, e := range page.Entries {
			seen[e.ID]++
			order = append(order, e.Message.Content[0].Text)
		}
		if !page.HasMore {
			break
		}
		cursor = page.Earliest
	}
	if len(seen) != 500 {
		t.Fatalf("paging covered %d of 500 entries", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("entry %s appeared %d times", id, n)
		}
	}
	// Pages come back oldest-first within a page, newest page first. So the
	// newest message is the last element of the first page, and the oldest is
	// the first element of the last page.
	if len(order) == 500 {
		if order[0] != "message number 450" {
			t.Fatalf("first page starts at %q, want the 450th message", order[0])
		}
		if order[49] != "message number 499" {
			t.Fatalf("first page ends at %q, want the newest message", order[49])
		}
		if order[len(order)-50] != "message number 0" {
			t.Fatalf("oldest message = %q, want the 0th", order[len(order)-50])
		}
	}
}

// A short session has nothing to page: one page, and no claim of more.
func TestShortSessionIsOnePageWithNoMore(t *testing.T) {
	path := transcriptOf(t, 12)
	page, err := PageBefore(path, "", DefaultPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 12 {
		t.Fatalf("got %d entries, want all 12", len(page.Entries))
	}
	if page.HasMore {
		t.Fatal("a 12-entry session has nothing before the first page")
	}
	if page.Earliest != page.Entries[0].ID || page.Leaf != page.Entries[len(page.Entries)-1].ID {
		t.Fatal("cursors should bracket the returned entries")
	}
}

// Paging past the start must return nothing rather than an error, because that
// is the normal end of a scrollback.
func TestPagingBeforeTheFirstEntryIsEmpty(t *testing.T) {
	path := transcriptOf(t, 20)
	first, err := PageBefore(path, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	// A 20-entry session paged ten at a time is exactly two pages.
	second, err := PageBefore(path, first.Earliest, 10)
	if err != nil {
		t.Fatalf("paging to the second page should not error: %v", err)
	}
	if len(second.Entries) != 10 {
		t.Fatalf("second page has %d entries, want 10", len(second.Entries))
	}
	if second.HasMore {
		t.Fatal("the second page of a 20-entry session has nothing behind it")
	}
	// Past the start is empty, and that is the normal end of a scrollback.
	third, err := PageBefore(path, second.Earliest, 10)
	if err != nil {
		t.Fatalf("paging past the start should not error: %v", err)
	}
	if len(third.Entries) != 0 {
		t.Fatalf("expected no entries past the start, got %d", len(third.Entries))
	}
}

// The whole point: opening a long session must not cost a full read.
func TestPagingALargeTranscriptDoesNotReadTheWholeFile(t *testing.T) {
	path := transcriptOf(t, 12000)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	page, err := PageBefore(path, "", 200)
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 200 {
		t.Fatalf("got %d entries, want 200", len(page.Entries))
	}

	full := time.Now()
	if _, err := ReadAll(path); err != nil {
		t.Fatal(err)
	}
	fullTook := time.Since(full)

	fmt.Printf("file %.1f MB: page(200) %v vs readAll %v (%.0fx)\n",
		float64(info.Size())/(1<<20), took.Round(time.Millisecond),
		fullTook.Round(time.Millisecond), float64(fullTook)/float64(took))
	if took > fullTook {
		t.Fatalf("paging (%v) should not be slower than reading everything (%v)", took, fullTook)
	}
}

// A session with one enormous entry must still return, rather than looping until
// the read window caps out.
func TestOneHugeEntryStillReturns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	store, err := Open(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	huge := make([]byte, 400*1024)
	for i := range huge {
		huge[i] = 'x'
	}
	for i := 0; i < 5; i++ {
		if _, err := store.Append(Entry{Type: TypeMessage, Message: &Message{
			Role: RoleUser, Content: []Block{{Type: BlockText, Text: string(huge)}},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan Page, 1)
	go func() {
		p, err := PageBefore(path, "", 200)
		if err != nil {
			t.Error(err)
		}
		done <- p
	}()
	select {
	case p := <-done:
		if len(p.Entries) != 5 {
			t.Fatalf("got %d entries, want 5", len(p.Entries))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("paging a session of huge entries did not finish")
	}
}

func TestPagingAnEmptyOrMissingSession(t *testing.T) {
	dir := t.TempDir()
	// Missing file: an error is fine, a hang is not.
	if _, err := PageBefore(filepath.Join(dir, "nope.jsonl"), "", 10); err == nil {
		t.Fatal("a missing session should report an error")
	}
	// Header-only session: no content entries yet.
	path := filepath.Join(dir, "empty.jsonl")
	store, err := Open(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	page, err := PageBefore(path, "", 10)
	if err != nil {
		t.Fatalf("an empty session should not error: %v", err)
	}
	if len(page.Entries) != 0 {
		t.Fatalf("expected no entries, got %d", len(page.Entries))
	}
	if page.HasMore {
		t.Fatal("an empty session has nothing more")
	}
	_ = idsOf(page)
}
