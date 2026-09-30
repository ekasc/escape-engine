package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// These are the three costs a turn actually pays: appending each message as it
// completes, reading a window back to render the transcript, and reading the
// session header. Everything else in a turn is provider wait time.
//
// The transcript sizes are not invented. A short turn is a question and an
// answer; the large one is a long session with the bulky tool output and
// screenshots that accumulate in real use.

func benchEntry(i int, body string) Entry {
	return Entry{
		Type: TypeMessage,
		Message: &Message{
			Role:      RoleAssistant,
			Content:   []Block{{Type: "text", Text: body}},
			Timestamp: NowMillis(),
		},
		ID: fmt.Sprintf("e%06d", i),
	}
}

const benchBody = "The turn ran a handful of tools, edited a few files, and then " +
	"explained what it changed and why the previous approach was wrong. " +
	"Repeated to approximate a paragraph of ordinary prose in a transcript."

// Seeds are built once into a directory that outlives the benchmarks, because
// b.TempDir is torn down as soon as one finishes. Building a 20k-entry file
// inside the benchmark also put tens of thousands of appends into the measured
// window, which swamped the read path and made a profile useless.
var (
	seedDir   string
	seedCache sync.Map
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "escape-bench-")
	if err != nil {
		panic(err)
	}
	seedDir = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func seedStore(b *testing.B, entries int) string {
	b.Helper()
	if v, ok := seedCache.Load(entries); ok {
		return v.(string)
	}
	path := filepath.Join(seedDir, fmt.Sprintf("session-%d.jsonl", entries))
	s, err := Open(path, seedDir)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	for i := range entries {
		if _, err := s.Append(benchEntry(i, benchBody)); err != nil {
			b.Fatalf("append %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		b.Fatalf("close: %v", err)
	}
	seedCache.Store(entries, path)
	return path
}

// Append is on the hot path of every single message: the assistant message is
// written to disk before its end event is published.
func BenchmarkStoreAppend(b *testing.B) {
	for _, size := range []int{64, 4096} {
		b.Run(fmt.Sprintf("session-%d", size), func(b *testing.B) {
			dir := b.TempDir()
			path := filepath.Join(dir, "session.jsonl")
			s, err := Open(path, dir)
			if err != nil {
				b.Fatalf("open: %v", err)
			}
			b.Cleanup(func() { s.Close() })
			// Grow the file to the target size first, so the benchmark measures
			// appending into a realistic file rather than an empty one.
			for i := range size {
				if _, err := s.Append(benchEntry(i, benchBody)); err != nil {
					b.Fatalf("seed append: %v", err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				if _, err := s.Append(benchEntry(size+i, benchBody)); err != nil {
					b.Fatalf("append: %v", err)
				}
			}
		})
	}
}

// PageBefore is what the shell calls to render the transcript, and again each
// time the user scrolls back. A tail window means it should not grow with the
// file, and this is the measurement that would catch it if it did.
func BenchmarkPageBefore(b *testing.B) {
	for _, size := range []int{200, 2000, 20000} {
		b.Run(fmt.Sprintf("session-%d", size), func(b *testing.B) {
			path := seedStore(b, size)
			info, err := ReadInfo(path)
			if err != nil {
				b.Fatalf("read info: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := PageBefore(path, "", DefaultPageSize); err != nil {
					b.Fatalf("page: %v", err)
				}
			}
			_ = info
		})
	}
}

// ReadInfo backs the session list and the header. It reads the file's head, so
// it should be flat in the file size too.
func BenchmarkReadInfo(b *testing.B) {
	for _, size := range []int{200, 20000} {
		b.Run(fmt.Sprintf("session-%d", size), func(b *testing.B) {
			path := seedStore(b, size)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := ReadInfo(path); err != nil {
					b.Fatalf("read info: %v", err)
				}
			}
		})
	}
}

// A whole-file scan, for contrast with the windowed read. This is the cost the
// tail window exists to avoid, so it is measured rather than assumed.
func BenchmarkFullFileScan(b *testing.B) {
	for _, size := range []int{2000, 20000} {
		b.Run(fmt.Sprintf("session-%d", size), func(b *testing.B) {
			path := seedStore(b, size)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				f, err := os.Open(path)
				if err != nil {
					b.Fatalf("open: %v", err)
				}
				buf := make([]byte, 64*1024)
				for {
					if _, err := f.Read(buf); err != nil {
						break
					}
				}
				f.Close()
			}
		})
	}
}
