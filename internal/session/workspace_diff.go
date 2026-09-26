package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// WorkspaceDiff returns the current staged, unstaged, and untracked changes.
func WorkspaceDiff(cwd string, paths ...string) (string, error) {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	runDiff := func(args ...string) ([]byte, error) {
		diffArgs := append([]string{}, args...)
		diffArgs = append(diffArgs, "--")
		diffArgs = append(diffArgs, paths...)
		cmd := exec.Command("git", append([]string{"-C", cwd, "diff", "--no-ext-diff"}, diffArgs...)...)
		return cmd.CombinedOutput()
	}
	unstaged, err := runDiff()
	if err != nil {
		detail := strings.TrimSpace(string(unstaged))
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git diff failed: %s", detail)
	}
	staged, err := runDiff("--cached")
	if err != nil {
		detail := strings.TrimSpace(string(staged))
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git diff --cached failed: %s", detail)
	}
	var out []byte
	if len(staged) > 0 {
		out = append(out, []byte("--- staged ---\n")...)
		out = append(out, staged...)
	}
	if len(unstaged) > 0 {
		if len(out) > 0 {
			out = append(out, '\n')
		}
		out = append(out, unstaged...)
	}

	untrackedArgs := []string{"-C", cwd, "ls-files", "--others", "--exclude-standard", "-z", "--"}
	untrackedArgs = append(untrackedArgs, paths...)
	untrackedCmd := exec.Command("git", untrackedArgs...)
	untracked, err := untrackedCmd.Output()
	if err != nil {
		detail := strings.TrimSpace(string(untracked))
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git untracked scan failed: %s", detail)
	}
	files := strings.Split(string(untracked), "\x00")
	const maxUntrackedDiffFiles = 20
	for i, file := range files {
		if file == "" {
			continue
		}
		if i >= maxUntrackedDiffFiles {
			out = append(out, []byte("\n…[additional untracked files omitted]")...)
			break
		}
		cmd := exec.Command("git", "-C", cwd, "diff", "--no-ext-diff", "--no-index", "--", "/dev/null", file)
		fileDiff, diffErr := cmd.CombinedOutput()
		if diffErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(diffErr, &exitErr) || exitErr.ExitCode() != 1 {
				detail := strings.TrimSpace(string(fileDiff))
				if detail == "" {
					detail = diffErr.Error()
				}
				return "", fmt.Errorf("git untracked diff failed: %s", detail)
			}
		}
		if len(fileDiff) == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, '\n')
		}
		out = append(out, []byte("--- untracked "+file+" ---\n")...)
		out = append(out, fileDiff...)
	}
	if len(out) == 0 {
		return "No uncommitted changes.", nil
	}
	const maxDiffOutput = 30_000
	if len(out) > maxDiffOutput {
		return string(out[:maxDiffOutput]) + "\n…[diff truncated]", nil
	}
	return string(out), nil
}
