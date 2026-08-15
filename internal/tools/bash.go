package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultBashTimeout = 30 * time.Second
	maxBashTimeout     = 10 * time.Minute
	maxBashOutput      = 30_000 // clamp before returning to the model
)

// Bash runs a shell command via bash -lc. Output is combined stdout+stderr
// with the exit code appended on failure. Timeouts and output size are
// bounded so a runaway command can't wedge the loop or bloat the session.
func Bash(cwd string) Tool {
	return Tool{
		Name:        "bash",
		Description: "Run a shell command in the workspace. Use for building, testing, git, and any non-file operation. Prefer targeted commands over long-running ones.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "The shell command to run"},
				"timeout": map[string]any{"type": "number", "description": "Timeout in seconds (default 30, max 600)"},
			},
			"required": []string{"command"},
		},
		Run: func(ctx context.Context, args map[string]any) Result {
			var p struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			}
			if err := decodeArgs(args, &p); err != nil {
				return errResult(err)
			}
			if strings.TrimSpace(p.Command) == "" {
				return errResult(fmt.Errorf("bash: empty command"))
			}

			timeout := defaultBashTimeout
			if p.Timeout > 0 {
				timeout = time.Duration(p.Timeout) * time.Second
			}
			if timeout > maxBashTimeout {
				timeout = maxBashTimeout
			}

			cmdCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			cmd := exec.CommandContext(cmdCtx, "bash", "-lc", p.Command)
			cmd.Dir = cwd
			cmd.Env = os.Environ()
			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &out
			err := cmd.Run()

			output := out.String()
			if cmdCtx.Err() == context.DeadlineExceeded {
				return Result{
					Output:  clampOutput(output) + fmt.Sprintf("\n[command timed out after %s]", timeout),
					IsError: true,
				}
			}
			if err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					code := ee.ExitCode()
					return Result{Output: clampOutput(output) + fmt.Sprintf("\nCommand exited with code %d", code), IsError: code != 0}
				}
				return Result{Output: clampOutput(output) + fmt.Sprintf("\nError running command: %v", err), IsError: true}
			}
			return Result{Output: clampOutput(output)}
		},
	}
}

func clampOutput(s string) string {
	if len(s) > maxBashOutput {
		return s[:maxBashOutput] + fmt.Sprintf("\n…[output truncated at %d bytes]", maxBashOutput)
	}
	return s
}
