// Package resources implements pi-compatible resource loading for the agent
// runtime: context files (AGENTS.md / CLAUDE.md / AGENTS.override.md), system
// prompt files (SYSTEM.md / APPEND_SYSTEM.md), agent skills (the Agent
// Skills standard, https://agentskills.io) and prompt templates. Discovery
// and validation follow pi's rules: lenient (warn-only) validation, skills
// without a description are skipped, first-found wins on name collisions.
package resources

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/ekasc/escape/engine/internal/settings"
)

// Skill is a discovered agent skill: a directory containing SKILL.md, or a
// root-level .md file in the user or project skills directory, with YAML
// frontmatter per the Agent Skills specification.
type Skill struct {
	Name                   string
	Description            string
	Path                   string // absolute path to SKILL.md (or the .md file)
	AllowedTools           []string
	DisableModelInvocation bool
	// Disabled is set by the loader when the user switched this skill off. It is
	// kept separate from DisableModelInvocation, which the skill file declares
	// for itself, so a user setting never overwrites an author's intent.
	Disabled bool
	// Location is "user", "project" or "path", for display.
	Location string
	Body     string // markdown body after the frontmatter, trimmed

	location string // "user", "project" or "path"
}

// PromptTemplate is a discovered prompt template: a .md file whose filename
// (sans extension) is the command name.
type PromptTemplate struct {
	Name         string
	Description  string
	ArgumentHint string
	Path         string // absolute path to the .md file
	Body         string // markdown body after the frontmatter, trimmed

	location string // "user", "project" or "path"
}

// Command describes a slash command for the RPC get_commands response.
type Command struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Source       string `json:"source"`             // "skill" or "prompt"
	Location     string `json:"location,omitempty"` // "user", "project" or "path"
	Path         string `json:"path,omitempty"`     // absolute path of the command source
	ArgumentHint string `json:"argumentHint,omitempty"`
}

// Loader discovers and caches pi resources for one working directory.
type Loader struct {
	cwd   string
	s     *settings.Settings
	quiet bool

	skills   []Skill
	skillsOK bool

	templates   []PromptTemplate
	templatesOK bool
}

// New creates a Loader for cwd using the given (already merged) settings.
// A nil settings argument falls back to the defaults.
func New(cwd string, s *settings.Settings) *Loader {
	if s == nil {
		s = settings.Defaults()
	}
	return &Loader{cwd: cwd, s: s}
}

// NewQuiet creates a Loader that suppresses non-fatal resource warnings. It
// is used by the full-screen TUI, where stderr would otherwise corrupt the
// alternate-screen interface.
func NewQuiet(cwd string, s *settings.Settings) *Loader {
	loader := New(cwd, s)
	loader.quiet = true
	return loader
}

// warnf reports a non-fatal discovery or validation warning to stderr.
func (l *Loader) warnf(format string, args ...any) {
	if l.quiet {
		return
	}
	log.Printf("resources: "+format, args...)
}

// GetCommands returns the slash commands for the RPC get_commands command:
// prompt templates (source "prompt") followed by skills (source "skill",
// named "skill:<name>").
func (l *Loader) GetCommands() []Command {
	var cmds []Command
	for _, t := range l.Templates() {
		cmds = append(cmds, Command{
			Name:         t.Name,
			Description:  t.Description,
			Source:       "prompt",
			Location:     t.location,
			Path:         t.Path,
			ArgumentHint: t.ArgumentHint,
		})
	}
	for _, s := range l.Skills() {
		// A skill the user switched off produces no command. Advertising it
		// here would put its description back into the command list, and the
		// command list is context.
		if s.Disabled {
			continue
		}
		cmds = append(cmds, Command{
			Name:        "skill:" + s.Name,
			Description: s.Description,
			Source:      "skill",
			Location:    s.location,
			Path:        s.Path,
		})
	}
	return cmds
}

// absPath returns the absolute, cleaned form of path.
func absPath(path string) string {
	a, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return a
}

// homeDir returns the user home directory, or "." when unavailable.
func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return "."
	}
	return h
}

// ancestorDirs returns every directory from cwd up to the filesystem root,
// cwd first.
func ancestorDirs(cwd string) []string {
	var dirs []string
	dir := absPath(cwd)
	for {
		dirs = append(dirs, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return dirs
}

// projectWalkDirs returns the directories from cwd up to the git root (the
// first ancestor containing a .git file or directory, as in worktrees) or
// the filesystem root when not in a repository, cwd first.
func projectWalkDirs(cwd string) []string {
	var dirs []string
	dir := absPath(cwd)
	for {
		dirs = append(dirs, dir)
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return dirs
}

// resolveResourcePath resolves a settings path (possibly relative to cwd or
// ~-prefixed) to an absolute path.
func resolveResourcePath(p, cwd string) string {
	if p == "~" || strings.HasPrefix(p, "~"+string(filepath.Separator)) {
		return filepath.Join(homeDir(), strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		return filepath.Join(cwd, p)
	}
	return p
}

// isFile reports whether path exists and is a regular file (following
// symlinks).
func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// isDir reports whether path exists and is a directory (following symlinks).
func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
