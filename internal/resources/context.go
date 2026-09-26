package resources

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/ekasc/escape-engine/internal/settings"
)

// contextFileCandidates are the per-directory context file names in
// precedence order; AGENTS.override.md replaces AGENTS.md/CLAUDE.md in the
// directory that contains it.
var contextFileCandidates = []string{
	"AGENTS.override.md",
	"AGENTS.md",
	"AGENTS.MD",
	"CLAUDE.md",
	"CLAUDE.MD",
}

// loadContextFileFromDir returns the first existing regular context file in
// dir.
func loadContextFileFromDir(dir string) (path, content string, ok bool) {
	for _, name := range contextFileCandidates {
		p := filepath.Join(dir, name)
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		return p, string(data), true
	}
	return "", "", false
}

// ContextFiles returns the layered context-file section for the system
// prompt: the global ~/.escape context file, then each directory from the
// filesystem root down to cwd (AGENTS.override.md replaces AGENTS.md and
// CLAUDE.md in its directory). The text uses pi's <project_context> format.
// Returns "" when no context files exist.
func (l *Loader) ContextFiles() string {
	type file struct{ path, content string }
	var files []file
	seen := make(map[string]bool)
	add := func(path, content string) {
		abs := absPath(path)
		if seen[abs] {
			return
		}
		seen[abs] = true
		files = append(files, file{path, content})
	}

	// Global instructions first.
	if path, content, ok := loadContextFileFromDir(settings.GlobalDir()); ok {
		add(path, content)
	}
	// Ancestor directories from the filesystem root down to cwd, so the
	// final order is global → root-most → ... → cwd.
	dirs := ancestorDirs(l.cwd)
	var ancestors []file
	for i := len(dirs) - 1; i >= 0; i-- {
		if path, content, ok := loadContextFileFromDir(dirs[i]); ok {
			abs := absPath(path)
			if !seen[abs] {
				seen[abs] = true
				ancestors = append(ancestors, file{path, content})
			}
		}
	}
	files = append(files, ancestors...)

	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n<project_context>\n\n")
	b.WriteString("Project-specific instructions and guidelines:\n\n")
	for _, f := range files {
		b.WriteString(`<project_instructions path="` + f.path + `">` + "\n")
		b.WriteString(f.content)
		b.WriteString("\n</project_instructions>\n\n")
	}
	b.WriteString("</project_context>\n")
	return b.String()
}

// SystemPrompt returns the system-prompt file content and whether it
// REPLACES the default prompt: <cwd>/.escape/SYSTEM.md or ~/.escape/SYSTEM.md
// replace (true); <cwd>/.escape/APPEND_SYSTEM.md or ~/.escape/APPEND_SYSTEM.md
// append to the default prompt (false). Project files win over global ones.
// Returns ("", false) when no file exists.
func (l *Loader) SystemPrompt() (string, bool) {
	if content, ok := readPromptFile(filepath.Join(absPath(l.cwd), ".escape", "SYSTEM.md")); ok {
		return content, true
	}
	if content, ok := readPromptFile(filepath.Join(settings.GlobalDir(), "SYSTEM.md")); ok {
		return content, true
	}
	if content, ok := readPromptFile(filepath.Join(absPath(l.cwd), ".escape", "APPEND_SYSTEM.md")); ok {
		return content, false
	}
	if content, ok := readPromptFile(filepath.Join(settings.GlobalDir(), "APPEND_SYSTEM.md")); ok {
		return content, false
	}
	return "", false
}

func readPromptFile(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(data), true
}
