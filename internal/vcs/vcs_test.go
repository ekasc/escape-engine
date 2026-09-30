package vcs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekasc/escape-engine/internal/session"
)

// newRepo builds a throwaway repository with one commit so tests exercise the
// same git plumbing the shell drives, rather than a mocked stand-in.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--quiet")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "--quiet", "-m", "initial")
	return dir
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The branch name comes from a rev-parse that now runs alongside the other
// three reads, so it is worth its own assertions: a lost goroutine result is
// silent, and the header chip would just stop appearing.
func TestStatusReportsBranchName(t *testing.T) {
	dir := newRepo(t)
	git(t, dir, "checkout", "-q", "-b", "feature/some-name")
	st, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.RefName != "feature/some-name" {
		t.Errorf("RefName = %q, want %q", st.RefName, "feature/some-name")
	}
}

func TestStatusDetachedHeadIsNotABranch(t *testing.T) {
	dir := newRepo(t)
	git(t, dir, "checkout", "-q", "--detach", "HEAD")
	st, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A detached HEAD reports the literal string "HEAD". Showing that in the
	// header would read as a branch called HEAD.
	if st.RefName != "" {
		t.Errorf("RefName = %q, want empty on a detached HEAD", st.RefName)
	}
	if !st.IsRepo {
		t.Error("a detached HEAD is still a repository")
	}
}

func TestStatusCountsAdditionsAndDeletions(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.txt", "one\ntwo\nthree\n")
	st, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Both numstat reads feed these totals, and both moved into goroutines.
	if st.Insertions != 1 || st.Deletions != 0 {
		t.Errorf("insertions/deletions = %d/%d, want 1/0", st.Insertions, st.Deletions)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestStatusCleanRepo(t *testing.T) {
	dir := newRepo(t)
	st, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.IsRepo {
		t.Fatal("expected IsRepo")
	}
	if st.HasChanges {
		t.Fatalf("expected no changes, got %+v", st)
	}
	if len(st.Staged) != 0 || len(st.Unstaged) != 0 {
		t.Fatalf("expected empty buckets, got %+v", st)
	}
}

func TestStatusUnstagedEdit(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.txt", "one\ntwo\nthree\n")

	st, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.HasChanges {
		t.Fatal("expected changes")
	}
	if len(st.Unstaged) != 1 || st.Unstaged[0].Path != "a.txt" {
		t.Fatalf("unexpected unstaged: %+v", st.Unstaged)
	}
	if st.Insertions != 1 {
		t.Errorf("expected 1 insertion, got %d", st.Insertions)
	}
}

func TestStatusSeparatesStagedFromUnstaged(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.txt", "one\ntwo\nstaged\n")
	if err := Stage(dir); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "a.txt", "one\ntwo\nstaged\nmore\n")
	write(t, dir, "b.txt", "new file\n")

	st, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Staged) != 1 || st.Staged[0].Path != "a.txt" {
		t.Fatalf("unexpected staged: %+v", st.Staged)
	}
	// a.txt is staged and then modified again, so it appears in both buckets
	// by design; b.txt is untracked and unstaged.
	unstaged := map[string]bool{}
	for _, f := range st.Unstaged {
		unstaged[f.Path] = true
	}
	if !unstaged["a.txt"] || !unstaged["b.txt"] {
		t.Fatalf("expected a.txt and b.txt unstaged, got %+v", st.Unstaged)
	}
}

func TestStatusOutsideRepoIsNotAnError(t *testing.T) {
	st, err := Read(t.TempDir())
	if err != nil {
		t.Fatalf("a non-repo must not error, got %v", err)
	}
	if st.IsRepo {
		t.Fatal("expected IsRepo false")
	}
	if st.HasChanges {
		t.Fatal("expected no changes outside a repo")
	}
}

func TestStageThenUnstage(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.txt", "changed\n")

	if err := Stage(dir); err != nil {
		t.Fatal(err)
	}
	st, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Staged) != 1 {
		t.Fatalf("expected a.txt staged, got %+v", st.Staged)
	}

	if err := Unstage(dir); err != nil {
		t.Fatal(err)
	}
	st, err = Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Staged) != 0 {
		t.Fatalf("expected nothing staged, got %+v", st.Staged)
	}
}

func TestStageSubset(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.txt", "a changed\n")
	write(t, dir, "b.txt", "b changed\n")

	if err := Stage(dir, "a.txt"); err != nil {
		t.Fatal(err)
	}
	st, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Staged) != 1 || st.Staged[0].Path != "a.txt" {
		t.Fatalf("expected only a.txt staged, got %+v", st.Staged)
	}
}

func TestCommitWritesIndex(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.txt", "committed\n")
	if err := Stage(dir); err != nil {
		t.Fatal(err)
	}
	if err := Commit(dir, "change a"); err != nil {
		t.Fatal(err)
	}
	st, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.HasChanges {
		t.Fatalf("expected a clean tree after commit, got %+v", st)
	}
}

func TestFileDiffReturnsAFileTheWholeTreeDiffTruncates(t *testing.T) {
	// The whole-tree diff is capped, so on a large change set it holds only the
	// first few files. Selecting a later file must still produce its patch, or
	// the file appears to have no changes at all.
	dir := newRepo(t)
	var late string
	for i := 0; i < 40; i++ {
		name := fmt.Sprintf("filler-%02d.txt", i)
		body := strings.Repeat("filler line\n", 400)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		late = name
	}
	write(t, dir, "a.txt", "changed\n")

	// Confirm the tree really is big enough to truncate WorkspaceDiff.
	tree, err := session.WorkspaceDiff(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tree, "diff truncated") {
		t.Skip("change set is not large enough to truncate; nothing to prove here")
	}
	if strings.Contains(tree, late) {
		t.Fatal("precondition failed: the late file should be missing from the tree diff")
	}

	// The late file's own diff still resolves.
	got, err := FileDiff(dir, late)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, late) {
		t.Fatalf("expected a patch naming %s, got:\n%s", late, got)
	}
	if !strings.Contains(got, "filler line") {
		t.Fatalf("expected the file's content, got:\n%s", got)
	}
}

func TestFileDiffCoversStagedAndUntracked(t *testing.T) {
	dir := newRepo(t)

	// Modified and unstaged.
	write(t, dir, "a.txt", "unstaged change\n")
	got, err := FileDiff(dir, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "unstaged change") {
		t.Fatalf("expected the unstaged edit, got:\n%s", got)
	}

	// Staged.
	write(t, dir, "a.txt", "staged change\n")
	if err := Stage(dir); err != nil {
		t.Fatal(err)
	}
	got, err = FileDiff(dir, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "staged change") {
		t.Fatalf("expected the staged edit, got:\n%s", got)
	}

	// Untracked: no HEAD to diff against, needs the --no-index path.
	write(t, dir, "new.txt", "brand new\n")
	got, err = FileDiff(dir, "new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "brand new") || !strings.Contains(got, "new.txt") {
		t.Fatalf("expected the untracked file's contents, got:\n%s", got)
	}
}

func TestFileDiffOnCleanFileIsEmpty(t *testing.T) {
	dir := newRepo(t)
	got, err := FileDiff(dir, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != "" {
		t.Fatalf("expected no diff for an unmodified file, got:\n%s", got)
	}
}

func TestFileDiffRejectsEmptyPath(t *testing.T) {
	dir := newRepo(t)
	if _, err := FileDiff(dir, "  "); err == nil {
		t.Fatal("expected an error for a blank path")
	}
}

func TestCommitRejectsEmptyMessage(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.txt", "x\n")
	if err := Stage(dir); err != nil {
		t.Fatal(err)
	}
	if err := Commit(dir, "   "); err == nil {
		t.Fatal("expected an error for an empty commit message")
	} else if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("unhelpful error: %v", err)
	}
}
