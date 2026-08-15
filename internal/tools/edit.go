package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Edit applies an opencode-style patch: replace one exact occurrence of
// oldText with newText. It refuses when oldText is absent or ambiguous,
// which forces the model to be precise.
func Edit(cwd string) Tool {
	return Tool{
		Name:        "edit",
		Description: "Replace one exact occurrence of oldText with newText in a file. Fails if oldText is not found or matches multiple locations. Use surrounding context to make oldText unique.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "File path (absolute or relative to the workspace)"},
				"oldText": map[string]any{"type": "string", "description": "Exact text to find and replace (must be unique)"},
				"newText": map[string]any{"type": "string", "description": "Replacement text"},
			},
			"required": []string{"path", "oldText", "newText"},
		},
		Run: func(ctx context.Context, args map[string]any) Result {
			var p struct {
				Path    string `json:"path"`
				OldText string `json:"oldText"`
				NewText string `json:"newText"`
			}
			if err := decodeArgs(args, &p); err != nil {
				return errResult(err)
			}
			path, err := resolve(cwd, p.Path)
			if err != nil {
				return errResult(err)
			}
			if p.OldText == "" {
				return errResult(fmt.Errorf("edit: oldText must not be empty"))
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return errResult(fmt.Errorf("edit %s: %w", path, err))
			}
			content := string(data)

			idx := strings.Index(content, p.OldText)
			if idx < 0 {
				return errResult(fmt.Errorf("edit %s: oldText not found", path))
			}
			rest := content[idx+len(p.OldText):]
			if strings.Contains(rest, p.OldText) {
				return errResult(fmt.Errorf("edit %s: oldText matches multiple locations; include more surrounding context", path))
			}

			line := strings.Count(content[:idx], "\n") + 1
			newContent := content[:idx] + p.NewText + rest
			if err := os.WriteFile(path, []byte(newContent), 0o644); err != nil {
				return errResult(fmt.Errorf("edit %s: %w", path, err))
			}

			oldLine := firstLine(p.OldText)
			newLine := firstLine(p.NewText)
			out := fmt.Sprintf("Edited %s (line %d)", path, line)
			if oldLine != "" && oldLine != newLine {
				out += fmt.Sprintf("\n- %s\n+ %s", oldLine, newLine)
			}
			return Result{Output: out}
		},
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
