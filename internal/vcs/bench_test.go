package vcs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// vcsRepo builds a repository of a given shape. The two shapes that matter are
// "clean" (what the panel shows most of the time) and "dirty" (what it costs
// once a turn has edited a few files), because git's work is dominated by
// diffing the tree, not by reading the header.
func vcsRepo(b *testing.B, files, modified int) string {
	b.Helper()
	dir := b.TempDir()
	run := func(args ...string) {
		b.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			b.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q")
	for i := range files {
		name := filepath.Join(dir, fmt.Sprintf("file%03d.go", i))
		body := "package main\n\n// " + strconv.Itoa(i) + "\n" + benchGoBody
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-qm", "seed")
	for i := range modified {
		name := filepath.Join(dir, fmt.Sprintf("file%03d.go", i))
		if err := os.WriteFile(name, []byte("package main\n\n// changed "+strconv.Itoa(i)+"\n"+benchGoBody), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	return dir
}

const benchGoBody = `func demo() string {
	return "a representative function body with a little logic in it, long enough that a diff has real lines to show"
}
`

// Read backs the Source Control panel, which refreshes at the end of every turn
// and on every refreshGit. It shells out to git, so this measures process
// spawn plus git's own work.
func BenchmarkVcsRead(b *testing.B) {
	for _, tc := range []struct {
		files, modified int
		label           string
	}{
		{50, 0, "clean-50"},
		{200, 0, "clean-200"},
		{50, 5, "dirty-50-5"},
		{200, 20, "dirty-200-20"},
	} {
		b.Run(tc.label, func(b *testing.B) {
			dir := vcsRepo(b, tc.files, tc.modified)
			if _, err := Read(dir); err != nil {
				b.Fatalf("warm: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := Read(dir); err != nil {
					b.Fatalf("read: %v", err)
				}
			}
		})
	}
}

// FileDiff backs selecting a file in the panel. It runs against a working tree,
// so both the staged and unstaged shapes are worth knowing.
func BenchmarkVcsFileDiff(b *testing.B) {
	for _, label := range []string{"unstaged", "staged"} {
		b.Run(label, func(b *testing.B) {
			dir := vcsRepo(b, 50, 5)
			target := filepath.Join(dir, "file000.go")
			if label == "staged" {
				cmd := exec.Command("git", "add", "file000.go")
				cmd.Dir = dir
				if out, err := cmd.CombinedOutput(); err != nil {
					b.Fatalf("stage: %v (%s)", err, out)
				}
			}
			if _, err := FileDiff(dir, "file000.go"); err != nil {
				b.Fatalf("warm: %v", err)
			}
			_ = target
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := FileDiff(dir, "file000.go"); err != nil {
					b.Fatalf("filediff: %v", err)
				}
			}
		})
	}
}
