package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSmallOutputIsKeptWhole(t *testing.T) {
	dir := t.TempDir()
	out, path, spilled := SpillToolOutput("hello\nworld", dir, "a.txt")
	if spilled || path != "" {
		t.Fatalf("small output must not be spilled: %v %q", spilled, path)
	}
	if out != "hello\nworld" {
		t.Fatalf("output was altered: %q", out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("nothing should have been written")
	}
}

// The single most important property: a truncated result must never look
// complete, or the model will act on an answer that was quietly cut.
func TestTruncatedOutputSaysItWasTruncated(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("a line of output that goes on and on\n", 5000)
	out, path, spilled := SpillToolOutput(big, dir, "a.txt")
	if !spilled {
		t.Fatal("large output should be spilled")
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("a truncated result must say so:\n%s", out[:200])
	}
	if len(out) >= len(big) {
		t.Fatal("the inline text should be smaller than the original")
	}
	if !strings.Contains(out, path) {
		t.Fatalf("the note must point at the spill file, got %q", path)
	}
	if !strings.Contains(out, "grep") {
		t.Fatal("the note should say how to recover the rest")
	}
}

func TestSpilledFileHoldsTheWholeOutput(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("distinctive-line\n", 6000)
	_, path, _ := SpillToolOutput(big, dir, "a.txt")
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the spill file must be readable: %v", err)
	}
	if string(saved) != big {
		t.Fatalf("the spill file lost content: %d bytes, want %d", len(saved), len(big))
	}
}

// Byte limits and line limits are both real; output with very long lines must
// still be bounded.
func TestLongLinesAreBoundedByBytes(t *testing.T) {
	dir := t.TempDir()
	oneLine := strings.Repeat("x", 200*1024)
	out, path, spilled := SpillToolOutput(oneLine, dir, "a.txt")
	if !spilled {
		t.Fatal("a 200KB single line should be spilled")
	}
	if len(out) > ToolOutputMaxBytes+2048 {
		t.Fatalf("inline output is %d bytes, want it bounded near %d", len(out), ToolOutputMaxBytes)
	}
	saved, _ := os.ReadFile(path)
	if string(saved) != oneLine {
		t.Fatal("the spill file must hold the original, not the bounded version")
	}
}

// Losing the spill must not mean losing the result, and must not produce
// something that reads as complete.
func TestFailedSpillStillMarksOutputAsTruncated(t *testing.T) {
	// A path that cannot be created: a file where a directory should be.
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("line\n", 5000)
	out, path, spilled := SpillToolOutput(big, blocked, "a.txt")
	if !spilled {
		t.Fatal("it should still report that it truncated")
	}
	if path != "" {
		t.Fatalf("no path should be claimed when the spill failed: %q", path)
	}
	if !strings.Contains(out, "truncated") || !strings.Contains(out, "could not be saved") {
		t.Fatalf("a failed spill must be stated, not hidden:\n%s", out[:200])
	}
}

func TestSpillFilesArePrivate(t *testing.T) {
	dir := t.TempDir()
	_, path, _ := SpillToolOutput(strings.Repeat("x\n", 5000), dir, "a.txt")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("spill file mode = %o, want 600", perm)
	}
}

func TestPruneRemovesOnlyExpiredSpillFiles(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "fresh.txt")
	old := filepath.Join(dir, "old.txt")
	if err := os.WriteFile(fresh, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-ToolOutputRetention - time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	if removed := PruneSpilledToolOutput(dir); removed != 1 {
		t.Fatalf("removed %d, want 1", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("the expired file should be gone")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("the fresh file must survive: %v", err)
	}
	// Pruning a directory that does not exist is normal, not an error.
	if removed := PruneSpilledToolOutput(filepath.Join(dir, "missing")); removed != 0 {
		t.Fatalf("pruning a missing directory should remove nothing, got %d", removed)
	}
}

func TestSpillHandlesEmptyAndTinyInput(t *testing.T) {
	dir := t.TempDir()
	for _, in := range []string{"", "\n", "a"} {
		out, _, spilled := SpillToolOutput(in, dir, "a.txt")
		if spilled {
			t.Fatalf("%q should not be spilled", in)
		}
		if out != in {
			t.Fatalf("%q was altered to %q", in, out)
		}
	}
}

// Two turns spilling at once must not overwrite each other.
func TestConcurrentSpillsKeepBothFiles(t *testing.T) {
	dir := t.TempDir()
	done := make(chan string, 8)
	for i := 0; i < 8; i++ {
		go func(i int) {
			_, path, _ := SpillToolOutput(
				strings.Repeat(fmt.Sprintf("line-%d\n", i), 5000),
				dir, fmt.Sprintf("tool-%d.txt", i))
			done <- path
		}(i)
	}
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		seen[<-done] = true
	}
	if len(seen) != 8 {
		t.Fatalf("got %d distinct spill files, want 8", len(seen))
	}
	for path := range seen {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("spill file %s missing: %v", path, err)
		}
	}
}
