// Package vcs reports and mutates the working tree of a git repository.
//
// Everything that runs git lives here rather than in the shell so that a
// write to the repository cannot bypass the engine's approval gate. The
// shell holds only presentation: which actions are offered, and why one is
// currently unavailable.
package vcs

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// FileStatus is the state of one path in the working tree. A path can appear
// in both Status.Staged and Status.Unstaged, which is what happens when a
// file is added and then modified again, so callers must not treat the two
// slices as a partition.
type FileStatus struct {
	Path       string `json:"path"`
	Insertions int    `json:"insertions"`
	Deletions  int    `json:"deletions"`
	Untracked  bool   `json:"untracked"`
}

// Status is the whole working-tree picture the sidebar renders from.
type Status struct {
	// IsRepo is false when cwd is not inside a git work tree. That is an
	// ordinary empty state, not an error, so it is reported here instead.
	IsRepo     bool         `json:"isRepo"`
	RefName    string       `json:"refName"`
	HasChanges bool         `json:"hasChanges"`
	Staged     []FileStatus `json:"staged"`
	Unstaged   []FileStatus `json:"unstaged"`
	Insertions int          `json:"insertions"`
	Deletions  int          `json:"deletions"`
}

// ErrNotARepo is returned by the mutating helpers when cwd is not a
// repository. Status folds this into IsRepo instead of surfacing it.
var ErrNotARepo = errors.New("not a git repository")

func run(cwd string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 {
			return "", ErrNotARepo
		}
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git %s failed: %s", args[0], detail)
	}
	return string(out), nil
}

// runAllowDiffExit is like run but treats exit code 1 as success. `git diff`
// exits 1 when the files differ, which for a diff request is the normal
// outcome, and the patch it printed to stdout is the answer.
func runAllowDiffExit(cwd string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if exitErr.ExitCode() == 1 {
				return string(out), nil
			}
			if exitErr.ExitCode() == 128 {
				return "", ErrNotARepo
			}
		}
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git %s failed: %s", args[0], detail)
	}
	return string(out), nil
}

// addNumstat applies `git diff --numstat` output onto the matching entries.
// Binary files report "-" in place of a count and are skipped.
func addNumstat(files []FileStatus, numstat string) {
	for _, line := range strings.Split(strings.TrimSpace(numstat), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		ins, err1 := strconv.Atoi(parts[0])
		del, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			continue
		}
		for i := range files {
			if files[i].Path == parts[2] {
				files[i].Insertions += ins
				files[i].Deletions += del
			}
		}
	}
}

// Read reports the working tree. The porcelain v1 format carries the
// staged/unstaged split and the untracked set in one pass, so the whole
// status is one subprocess plus the two numstat reads that supply counts.
func Read(cwd string) (*Status, error) {
	if cwd == "" {
		dir, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		cwd = dir
	}

	st := &Status{Staged: []FileStatus{}, Unstaged: []FileStatus{}}

	// The four reads below are independent and read-only, and each costs about
	// 5ms almost entirely in process spawn rather than in git's own work. Run
	// them together: the panel refreshes at the end of every turn, so four
	// sequential spawns put ~25ms between the agent finishing and the UI
	// settling, for a command set whose total work is under 7ms.
	//
	// The status read still gates the result — a directory that is not a repo
	// reports an empty status — but it no longer gates the other three. In
	// that case their output is discarded. Those extra spawns only happen
	// outside a repository, where the panel is already showing an empty state.
	var (
		wg           sync.WaitGroup
		out          string
		statusErr    error
		branch       string
		unstagedStat string
		stagedStat   string
	)
	wg.Add(4)
	go func() {
		defer wg.Done()
		out, statusErr = run(cwd, "status", "--porcelain", "--untracked-files=all")
	}()
	go func() {
		defer wg.Done()
		if b, err := run(cwd, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
			branch = b
		}
	}()
	go func() {
		defer wg.Done()
		if o, err := run(cwd, "diff", "--numstat"); err == nil {
			unstagedStat = o
		}
	}()
	go func() {
		defer wg.Done()
		if o, err := run(cwd, "diff", "--numstat", "--cached"); err == nil {
			stagedStat = o
		}
	}()
	wg.Wait()

	if statusErr != nil {
		if errors.Is(statusErr, ErrNotARepo) {
			return st, nil
		}
		return nil, statusErr
	}
	st.IsRepo = true

	if name := strings.TrimSpace(branch); name != "" && name != "HEAD" {
		// A detached HEAD reports the literal "HEAD", which is not a branch.
		st.RefName = name
	}

	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		index, worktree := line[0], line[1]
		path := strings.TrimSpace(line[3:])
		if path == "" {
			continue
		}
		// A rename reads "old -> new"; the new path is the one to stage.
		if idx := strings.Index(path, " -> "); idx >= 0 {
			path = path[idx+4:]
		}
		// Porcelain carries two independent columns: the index and the
		// worktree. "MM" means the path is staged *and* modified again, so it
		// legitimately belongs in both buckets. An earlier version used a
		// switch, which sent such a path to only the first matching bucket.
		untracked := index == '?' && worktree == '?'
		if untracked {
			st.Unstaged = append(st.Unstaged, FileStatus{Path: path, Untracked: true})
		}
		if index != ' ' && index != '?' {
			st.Staged = append(st.Staged, FileStatus{Path: path})
		}
		if worktree != ' ' && !untracked {
			st.Unstaged = append(st.Unstaged, FileStatus{Path: path})
		}
	}

	addNumstat(st.Unstaged, unstagedStat)
	addNumstat(st.Staged, stagedStat)

	for _, f := range st.Staged {
		st.Insertions += f.Insertions
		st.Deletions += f.Deletions
	}
	for _, f := range st.Unstaged {
		st.Insertions += f.Insertions
		st.Deletions += f.Deletions
	}
	st.HasChanges = len(st.Staged) > 0 || len(st.Unstaged) > 0
	return st, nil
}

// Stage adds paths to the index. With no paths it stages everything, which is
// what the "Stage all" action does.
func Stage(cwd string, paths ...string) error {
	args := append([]string{"add", "--"}, paths...)
	if len(paths) == 0 {
		args = []string{"add", "-A", "--"}
	}
	_, err := run(cwd, args...)
	return err
}

// Unstage removes paths from the index. A path that was never committed has
// no HEAD to reset against, so those fall back to `rm --cached`.
func Unstage(cwd string, paths ...string) error {
	args := append([]string{"reset", "--quiet", "HEAD", "--"}, paths...)
	if len(paths) == 0 {
		args = []string{"reset", "--quiet", "HEAD", "--"}
	}
	if _, err := run(cwd, args...); err != nil {
		if errors.Is(err, ErrNotARepo) {
			return err
		}
		// An unborn HEAD (no commits yet) has nothing to reset to. Dropping the
		// index entries outright is the equivalent operation there.
		fallback := append([]string{"rm", "--cached", "-r", "-q", "--"}, paths...)
		if len(paths) == 0 {
			fallback = []string{"rm", "--cached", "-r", "-q", "--", "."}
		}
		_, err2 := run(cwd, fallback...)
		return err2
	}
	return nil
}

// Commit writes the index. paths, when non-empty, commits only those paths
// regardless of what else is staged, which is a partial commit.
func Commit(cwd, message string, paths ...string) error {
	if strings.TrimSpace(message) == "" {
		return errors.New("commit message is empty")
	}
	args := []string{"commit", "--quiet", "--message", message}
	if len(paths) > 0 {
		args = append(args, append([]string{"--"}, paths...)...)
	}
	_, err := run(cwd, args...)
	return err
}
