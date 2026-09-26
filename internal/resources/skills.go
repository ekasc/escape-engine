package resources

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ekasc/escape/engine/internal/settings"
)

const (
	maxSkillNameLength        = 64
	maxSkillDescriptionLength = 1024
)

var skillNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// validateSkillName returns warning messages for name violations per the
// Agent Skills specification. Violations warn only; the skill still loads.
func validateSkillName(name string) []string {
	var errs []string
	if len(name) > maxSkillNameLength {
		errs = append(errs, fmt.Sprintf("name exceeds %d characters (%d)", maxSkillNameLength, len(name)))
	}
	if !skillNameRe.MatchString(name) {
		errs = append(errs, "name contains invalid characters (must be lowercase a-z, 0-9, hyphens only)")
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		errs = append(errs, "name must not start or end with a hyphen")
	}
	if strings.Contains(name, "--") {
		errs = append(errs, "name must not contain consecutive hyphens")
	}
	return errs
}

// Skills returns all discovered skills in discovery order. The name comes
// from the frontmatter, falling back to the parent directory name. Skills
// with a missing or empty description are skipped (a warning is logged);
// every other validation issue warns only. First-found wins on name
// collisions. The result is cached; create a new Loader to re-discover.
func (l *Loader) Skills() []Skill {
	if l.skillsOK {
		return l.skills
	}
	var skills []Skill
	seen := make(map[string]bool)
	add := func(s Skill) {
		if seen[s.Name] {
			l.warnf("skill name %q collision, keeping %s", s.Name, s.Path)
			return
		}
		seen[s.Name] = true
		skills = append(skills, s)
	}

	userSkillsDir := filepath.Join(settings.GlobalDir(), "skills")
	userAgentsSkillsDir := filepath.Join(homeDir(), ".agents", "skills")

	// Global: ~/.pi/agent/skills. Direct root .md files count as skills.
	l.scanSkillDir(userSkillsDir, true, "user", add)
	// Global: ~/.agents/skills. Root .md files are ignored.
	l.scanSkillDir(userAgentsSkillsDir, false, "user", add)
	// Project: <cwd>/.pi/skills. Direct root .md files count as skills.
	l.scanSkillDir(filepath.Join(absPath(l.cwd), ".pi", "skills"), true, "project", add)
	// Project: <dir>/.agents/skills walking up from cwd to the git root or
	// the filesystem root. The user-level ~/.agents/skills directory is
	// skipped (it was already loaded above).
	for _, dir := range projectWalkDirs(l.cwd) {
		cand := filepath.Join(dir, ".agents", "skills")
		if filepath.Clean(cand) == filepath.Clean(userAgentsSkillsDir) {
			continue
		}
		l.scanSkillDir(cand, false, "project", add)
	}
	// Settings: explicit skill paths (files or directories).
	for _, p := range l.s.Skills {
		path := resolveResourcePath(p, l.cwd)
		st, err := os.Stat(path)
		if err != nil {
			l.warnf("skill path does not exist: %s", path)
			continue
		}
		if st.IsDir() {
			l.scanSkillDir(path, true, "path", add)
		} else if strings.HasSuffix(path, ".md") {
			if s, ok := l.loadSkillFile(path, "path"); ok {
				add(s)
			}
		} else {
			l.warnf("skill path is not a markdown file: %s", path)
		}
	}

	l.skills = skills
	l.skillsOK = true
	return skills
}

// scanSkillDir discovers skills under dir following pi's rules: a directory
// containing SKILL.md directly is a skill root and is not recursed into;
// otherwise subdirectories are scanned recursively and, when rootMD is set,
// direct root-level .md files count as individual skills. Hidden entries and
// node_modules are skipped.
func (l *Loader) scanSkillDir(dir string, rootMD bool, location string, add func(Skill)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // missing or unreadable: nothing to discover
	}
	// First pass: a direct SKILL.md makes this directory a skill root.
	for _, e := range entries {
		if e.Name() != "SKILL.md" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if !isFile(path) {
			continue
		}
		if s, ok := l.loadSkillFile(path, location); ok {
			add(s)
		}
		return // do not recurse into a skill root
	}
	// Second pass: recurse into subdirectories; load root .md files when
	// the caller allows them.
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}
		path := filepath.Join(dir, name)
		switch {
		case isDir(path):
			l.scanSkillDir(path, false, location, add)
		case rootMD && strings.HasSuffix(name, ".md") && isFile(path):
			if s, ok := l.loadSkillFile(path, location); ok {
				add(s)
			}
		}
	}
}

// loadSkillFile parses one skill file with lenient agentskills.io
// validation: a missing or empty description skips the skill; name and
// description length violations warn only. The name falls back to the parent
// directory name when the frontmatter has none.
func (l *Loader) loadSkillFile(path, location string) (Skill, bool) {
	content, err := os.ReadFile(path)
	if err != nil {
		l.warnf("failed to read skill file %s: %v", path, err)
		return Skill{}, false
	}
	fm, body := parseFrontmatter(string(content))
	description, _ := fm["description"].(string)
	if strings.TrimSpace(description) == "" {
		l.warnf("skill %s: description is required, skipping", path)
		return Skill{}, false
	}
	name, _ := fm["name"].(string)
	if name == "" {
		name = filepath.Base(filepath.Dir(path))
	}
	for _, msg := range validateSkillName(name) {
		l.warnf("skill %s: %s", path, msg)
	}
	if len(description) > maxSkillDescriptionLength {
		l.warnf("skill %s: description exceeds %d characters (%d)", path, maxSkillDescriptionLength, len(description))
	}
	full := absPath(path)
	return Skill{
		Name:                   name,
		Description:            description,
		Path:                   full,
		AllowedTools:           parseAllowedTools(fm["allowed-tools"]),
		DisableModelInvocation: fm["disable-model-invocation"] == true,
		Disabled:               l.skillDisabled(full),
		Location:               location,
		Body:                   body,
		location:               location,
	}, true
}

// skillDisabled reports whether this skill is off for the project being loaded.
//
// The global list applies everywhere, and a project decision beats it. Absence
// from the override list is not a decision, it is an inheritance, which is what
// makes the state three valued rather than two: inherit, force on, force off.
// Paths are compared cleaned so a settings file written with a trailing
// separator still matches.
func (l *Loader) skillDisabled(path string) bool {
	if l.s == nil {
		return false
	}
	clean := filepath.Clean(path)
	for _, o := range l.s.SkillOverrides {
		if filepath.Clean(strings.TrimSpace(o.Path)) == clean {
			return !o.Enabled
		}
	}
	for _, p := range l.s.DisabledSkills {
		if filepath.Clean(strings.TrimSpace(p)) == clean {
			return true
		}
	}
	return false
}

// parseAllowedTools normalizes the allowed-tools frontmatter value (a
// space-delimited string or a YAML list) into a string slice.
func parseAllowedTools(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		var out []string
		for _, item := range t {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		return strings.Fields(t)
	}
	return nil
}

// SkillsXML formats the visible skills (those without
// disable-model-invocation) as an agentskills.io catalog for the system
// prompt, using the same structure pi emits. Returns "" when there are no
// visible skills.
func (l *Loader) SkillsXML() string {
	var visible []Skill
	for _, s := range l.Skills() {
		// A skill the user switched off is not offered to the model at all.
		// Advertising it and then refusing to load it would make the toggle a
		// lie, and would keep paying for the tokens.
		if !s.DisableModelInvocation && !s.Disabled {
			visible = append(visible, s)
		}
	}
	if len(visible) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nThe following skills provide specialized instructions for specific tasks.\n")
	b.WriteString("Use the read tool to load a skill's file when the task matches its description.\n")
	b.WriteString("When a skill file references a relative path, resolve it against the skill directory (parent of SKILL.md / dirname of the path) and use that absolute path in tool commands.\n")
	b.WriteString("\n<available_skills>\n")
	for _, s := range visible {
		fmt.Fprintf(&b, "  <skill>\n    <name>%s</name>\n    <description>%s</description>\n    <location>%s</location>\n  </skill>\n",
			escapeXML(s.Name), escapeXML(summarise(s.Description, promptDescriptionCap)), escapeXML(s.Path))
	}
	b.WriteString("</available_skills>")
	return b.String()
}

func escapeXML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// promptDescriptionCap bounds the description carried in the system prompt.
//
// A skill file is the source of truth and the model is told to read it when the
// description looks relevant, so the description only has to make that decision.
// An uncapped list of long prose descriptions was the single largest line item
// in every request: 232 skills, 72KB of description, and a median of 255
// characters each. Capping at 200 keeps the overwhelming majority of them whole
// and costs about 7,600 tokens per request.
const promptDescriptionCap = 200

// summarise trims to a word boundary so a shortened description still reads as a
// sentence rather than stopping mid-word.
func summarise(text string, limit int) string {
	flat := strings.Join(strings.Fields(text), " ")
	if len(flat) <= limit {
		return flat
	}
	cut := flat[:limit]
	if i := strings.LastIndexByte(cut, ' '); i > limit/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,.;:") + "..."
}

// SkillCommand expands a /skill:<name> command into the full skill content
// wrapped in a <skill> block, with the arguments appended as "User: <args>".
// The SKILL.md is re-read at expansion time so edits are picked up. Returns
// ("", false) when the skill is unknown, unreadable, or switched off.
//
// A disabled skill is refused here as well as being kept out of SkillsXML and
// the command list, because this is the path that puts a skill's entire body
// into the conversation. Filtering the listing alone would still let a disabled
// skill be injected by name.
func (l *Loader) SkillCommand(name, args string) (string, bool) {
	for _, s := range l.Skills() {
		if s.Name != name {
			continue
		}
		if s.Disabled {
			l.warnf("skill %s is switched off and was not loaded", s.Name)
			return "", false
		}
		content, err := os.ReadFile(s.Path)
		if err != nil {
			l.warnf("failed to read skill file %s: %v", s.Path, err)
			return "", false
		}
		_, body := parseFrontmatter(string(content))
		body = strings.TrimSpace(body)
		var b strings.Builder
		fmt.Fprintf(&b, `<skill name="%s" location="%s">`+"\n", s.Name, s.Path)
		fmt.Fprintf(&b, "References are relative to %s.\n\n", filepath.Dir(s.Path))
		b.WriteString(body)
		b.WriteString("\n</skill>")
		if args = strings.TrimSpace(args); args != "" {
			b.WriteString("\n\nUser: " + args)
		}
		return b.String(), true
	}
	return "", false
}
