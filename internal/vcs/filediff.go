package vcs

import (
	"fmt"
	"strings"
)

// maxFileDiffOutput caps a single file's patch. The whole-tree diff is capped
// far lower, so a large change set truncates before its later files are
// reachable at all; one file on its own always fits under this.
const maxFileDiffOutput = 400_000

// FileDiff returns the patch for a single path: its unstaged changes, its
// staged changes, or both, matching which bucket the file is listed in.
//
// Selecting one file has to work independently of the whole-tree diff. That
// diff is truncated to a few files once a change set gets large, so a file the
// user clicked may be absent from it entirely and appear to have no changes.
func FileDiff(cwd, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path is required")
	}
	// Untracked files have no HEAD to diff against, so they need the
	// --no-index form against /dev/null, the same way WorkspaceDiff does it.
	untracked, err := isUntracked(cwd, path)
	if err == nil && untracked {
		// Exit 1 is the normal "files differ" answer, so its output is kept.
		out, err := runAllowDiffExit(cwd, "diff", "--no-ext-diff", "--no-index", "--", "/dev/null", path)
		if err != nil {
			return "", err
		}
		return capFileDiff(strings.TrimRight(out, "\n")), nil
	}

	var parts []string
	if staged, err := run(cwd, "diff", "--no-ext-diff", "--cached", "--", path); err == nil {
		if strings.TrimSpace(staged) != "" {
			parts = append(parts, strings.TrimRight(staged, "\n"))
		}
	}
	if unstaged, err := run(cwd, "diff", "--no-ext-diff", "--", path); err == nil {
		if strings.TrimSpace(unstaged) != "" {
			parts = append(parts, strings.TrimRight(unstaged, "\n"))
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	return capFileDiff(strings.Join(parts, "\n")), nil
}

func capFileDiff(s string) string {
	if len(s) > maxFileDiffOutput {
		return s[:maxFileDiffOutput] + "\n…[file diff truncated]"
	}
	return s
}

// isUntracked reports whether git has never heard of this path.
func isUntracked(cwd, path string) (bool, error) {
	if _, err := run(cwd, "ls-files", "--error-unmatch", "--", path); err == nil {
		return false, nil
	}
	listed, err := run(cwd, "ls-files", "--others", "--exclude-standard", "--", path)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(listed) != "", nil
}
