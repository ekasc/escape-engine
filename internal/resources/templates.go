package resources

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/ekasc/escape-engine/internal/settings"
)

const templateDescriptionMax = 60

// Templates returns all discovered prompt templates: ~/.escape/prompts
// (user) and <cwd>/.escape/prompts (project), both non-recursive, followed by
// explicit paths from settings (files, or directories collected recursively).
// First-found wins on name collisions. The result is cached; create a new
// Loader to re-discover.
func (l *Loader) Templates() []PromptTemplate {
	if l.templatesOK {
		return l.templates
	}
	var templates []PromptTemplate
	seen := make(map[string]bool)
	add := func(t PromptTemplate) {
		if seen[t.Name] {
			return
		}
		seen[t.Name] = true
		templates = append(templates, t)
	}

	scanTemplatesDir(filepath.Join(settings.GlobalDir(), "prompts"), "user", add)
	scanTemplatesDir(filepath.Join(absPath(l.cwd), ".escape", "prompts"), "project", add)

	for _, p := range l.s.Prompts {
		path := resolveResourcePath(p, l.cwd)
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		if st.IsDir() {
			_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
					return nil
				}
				if t, ok := loadTemplate(p, "path"); ok {
					add(t)
				}
				return nil
			})
		} else if strings.HasSuffix(path, ".md") {
			if t, ok := loadTemplate(path, "path"); ok {
				add(t)
			}
		}
	}

	l.templates = templates
	l.templatesOK = true
	return templates
}

// scanTemplatesDir loads *.md files directly under dir (non-recursive).
func scanTemplatesDir(dir, location string, add func(PromptTemplate)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if t, ok := loadTemplate(filepath.Join(dir, e.Name()), location); ok {
			add(t)
		}
	}
}

// loadTemplate parses a prompt template: the filename (sans .md) is the
// command name, the description comes from the frontmatter or falls back to
// the first non-empty body line (truncated to 60 characters), and
// argument-hint is optional frontmatter.
func loadTemplate(path, location string) (PromptTemplate, bool) {
	content, err := os.ReadFile(path)
	if err != nil {
		return PromptTemplate{}, false
	}
	fm, body := parseFrontmatter(string(content))
	name := strings.TrimSuffix(filepath.Base(path), ".md")
	description, _ := fm["description"].(string)
	if description == "" {
		description = firstLine(body, templateDescriptionMax)
	}
	hint, _ := fm["argument-hint"].(string)
	return PromptTemplate{
		Name:         name,
		Description:  description,
		ArgumentHint: hint,
		Path:         absPath(path),
		Body:         body,
		location:     location,
	}, true
}

// firstLine returns the first non-empty line of s, trimmed, truncated to max
// runes with an ellipsis when longer.
func firstLine(s string, max int) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		r := []rune(line)
		if len(r) > max {
			return string(r[:max]) + "..."
		}
		return line
	}
	return ""
}

// ExpandTemplate expands a prompt template by name with the given argument
// string, supporting $1, $2, ..., $@/$ARGUMENTS, ${N:-default},
// ${@:-default}/${ARGUMENTS:-default}, ${@:N} and ${@:N:L}. Arguments are
// parsed bash-style (quoted strings). Returns ("", false) when the template
// is unknown.
func (l *Loader) ExpandTemplate(name, args string) (string, bool) {
	for _, t := range l.Templates() {
		if t.Name == name {
			return substituteArgs(t.Body, parseCommandArgs(args)), true
		}
	}
	return "", false
}

// parseCommandArgs splits an argument string bash-style, honoring single and
// double quotes.
func parseCommandArgs(argsString string) []string {
	var args []string
	var cur strings.Builder
	var inQuote byte
	for _, r := range argsString {
		if inQuote != 0 {
			if byte(r) == inQuote {
				inQuote = 0
			} else {
				cur.WriteRune(r)
			}
			continue
		}
		switch {
		case r == '"' || r == '\'':
			inQuote = byte(r)
		case unicode.IsSpace(r):
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}

// argSubRe matches argument placeholders, in precedence order:
// ${N:-default} / ${@:-default} / ${ARGUMENTS:-default}, then ${@:N(:L)},
// then the simple $N, $@, $ARGUMENTS.
var argSubRe = regexp.MustCompile(`\$\{(\d+|ARGUMENTS|@):-([^}]*)\}|\$\{@:(\d+)(?::(\d+))?\}|\$(ARGUMENTS|@|\d+)`)

// substituteArgs replaces argument placeholders in template content. Values
// containing placeholder-like patterns are not recursively substituted.
func substituteArgs(content string, args []string) string {
	allArgs := strings.Join(args, " ")
	return argSubRe.ReplaceAllStringFunc(content, func(match string) string {
		m := argSubRe.FindStringSubmatch(match)
		if m[1] != "" { // ${N:-default} | ${@:-default} | ${ARGUMENTS:-default}
			value := allArgs
			if m[1] != "@" && m[1] != "ARGUMENTS" {
				n, _ := strconv.Atoi(m[1])
				if n >= 1 && n <= len(args) {
					value = args[n-1]
				} else {
					value = ""
				}
			}
			if value != "" {
				return value
			}
			return m[2]
		}
		if m[3] != "" { // ${@:N} | ${@:N:L}
			start, _ := strconv.Atoi(m[3])
			start-- // 1-indexed; 0 is treated as 1 (bash convention)
			if start < 0 {
				start = 0
			}
			if start > len(args) {
				start = len(args)
			}
			if m[4] != "" {
				length, _ := strconv.Atoi(m[4])
				end := start + length
				if end > len(args) {
					end = len(args)
				}
				return strings.Join(args[start:end], " ")
			}
			return strings.Join(args[start:], " ")
		}
		// $N | $@ | $ARGUMENTS
		if m[5] == "@" || m[5] == "ARGUMENTS" {
			return allArgs
		}
		n, _ := strconv.Atoi(m[5])
		if n >= 1 && n <= len(args) {
			return args[n-1]
		}
		return ""
	})
}
