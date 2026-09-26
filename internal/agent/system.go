package agent

import "github.com/ekasc/escape/engine/internal/resources"

// buildSystemPrompt assembles the full system prompt for a session: the base
// coding-agent prompt, then SYSTEM.md (replace) / APPEND_SYSTEM.md (append),
// then layered context files (AGENTS.md/CLAUDE.md), the project's durable memory
// snapshot, and the skills listing.
func buildSystemPrompt(o Options) string {
	base := o.SystemPrompt
	loader := resources.New(o.Cwd, o.Settings)
	if o.QuietResourceWarnings {
		loader = resources.NewQuiet(o.Cwd, o.Settings)
	}
	if sys, replace := loader.SystemPrompt(); sys != "" {
		if replace {
			base = sys
		} else {
			base += "\n\n" + sys
		}
	}
	base += loader.ContextFiles()
	// Durable memory sits with the other project context and, like it, is read
	// once at agent build. Writes during the session land on disk and show up in
	// the next session's snapshot.
	base += o.Memory
	base += loader.SkillsXML()
	return base
}
