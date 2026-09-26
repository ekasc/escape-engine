package resources

import (
	"regexp"
	"strconv"
	"strings"
)

// parseFrontmatter splits markdown content into its YAML frontmatter (a
// string-keyed map) and the markdown body after the closing delimiter,
// trimmed. The delimiter rules mirror pi: the content must start with "---"
// and the frontmatter ends at the first "\n---" line. When no frontmatter is
// present the map is empty and the whole (normalized) content is the body.
func parseFrontmatter(content string) (map[string]any, string) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	if !strings.HasPrefix(normalized, "---") {
		return map[string]any{}, normalized
	}
	end := strings.Index(normalized[3:], "\n---")
	if end == -1 {
		return map[string]any{}, normalized
	}
	end += 3
	yamlString := normalized[4:end]
	body := strings.TrimSpace(normalized[end+4:])
	return parseYAML(yamlString), body
}

// parseYAML parses the subset of YAML that appears in skill and prompt
// template frontmatter: "key: value" mappings with plain, single/double
// quoted, flow-list and block-list values, literal (|) and folded (>)
// block scalars with chomping indicators, comments, and indented multi-line
// plain values. Unknown constructs degrade gracefully to strings; nothing
// here returns an error.
func parseYAML(s string) map[string]any {
	out := make(map[string]any)
	lines := strings.Split(s, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, rest, ok := splitKeyValue(trimmed)
		if !ok {
			continue
		}
		// All multi-line parsers return the index of the first unconsumed
		// line; step back one so the loop increment lands on it.
		if v, next, ok := parseBlockScalar(lines, i, rest); ok {
			out[key] = v
			i = next - 1
			continue
		}
		if rest == "" {
			if v, next, ok := parseIndentedList(lines, i); ok {
				out[key] = v
				i = next - 1
				continue
			}
			if v, next, ok := parseIndentedPlain(lines, i); ok {
				out[key] = v
				i = next - 1
				continue
			}
			out[key] = ""
			continue
		}
		v := parseScalar(rest)
		out[key] = v
		// Plain (unquoted, non-flow) values may continue on indented lines.
		if vs, ok := v.(string); ok && !quotedOrFlow(rest) {
			if cont, next, ok := parseIndentedPlain(lines, i); ok {
				if cont != "" {
					out[key] = vs + " " + cont
				}
				i = next - 1
			}
		}
	}
	return out
}

var keyRe = regexp.MustCompile(`^([A-Za-z0-9_-]+)\s*:\s*(.*)$`)

// splitKeyValue splits a frontmatter line into its key and the raw value
// (with leading whitespace stripped). The second return is false for lines
// that are not key: value mappings.
func splitKeyValue(trimmed string) (key, rest string, ok bool) {
	m := keyRe.FindStringSubmatch(trimmed)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// blockHeaderRe matches block scalar headers: >, |, with optional chomping
// indicator (+/-) and explicit indentation indicator (1-9).
var blockHeaderRe = regexp.MustCompile(`^([>|])([+-]?)([1-9]?)$`)

// parseBlockScalar parses a literal (|) or folded (>) block scalar starting
// at the line holding the header. It returns the value and the index after
// the consumed lines, or ok=false when rest is not a block scalar header.
func parseBlockScalar(lines []string, i int, rest string) (string, int, bool) {
	m := blockHeaderRe.FindStringSubmatch(rest)
	if m == nil {
		return "", i, false
	}
	literal := m[1] == "|"
	chomp := m[2]
	explicitIndent := m[3] != ""
	indent := 0
	if explicitIndent {
		indent, _ = strconv.Atoi(m[3])
	}

	j := i + 1
	minIndent := -1
	var raw []string
	for j < len(lines) {
		line := lines[j]
		if strings.TrimSpace(line) == "" {
			raw = append(raw, "")
			j++
			continue
		}
		ind := indentOf(line)
		if ind == 0 {
			break
		}
		if explicitIndent && ind < indent {
			break
		}
		if minIndent == -1 || ind < minIndent {
			minIndent = ind
		}
		raw = append(raw, line)
		j++
	}
	effIndent := indent
	if !explicitIndent {
		if minIndent == -1 {
			return applyChomp("", chomp), j, true // empty block scalar
		}
		effIndent = minIndent
	}

	content := make([]string, 0, len(raw))
	for _, line := range raw {
		if strings.TrimSpace(line) == "" {
			content = append(content, "")
			continue
		}
		if len(line) >= effIndent {
			content = append(content, strings.TrimRight(line[effIndent:], " \t"))
		} else {
			content = append(content, "")
		}
	}

	var value string
	if literal {
		value = strings.Join(content, "\n")
	} else {
		// Folded: single line breaks fold to spaces, each blank line stays a
		// line break.
		var b strings.Builder
		prevBlank := true
		for _, ln := range content {
			if ln == "" {
				b.WriteByte('\n')
				prevBlank = true
				continue
			}
			if !prevBlank {
				b.WriteByte(' ')
			}
			b.WriteString(ln)
			prevBlank = false
		}
		value = b.String()
	}
	// Block scalars always end with a line break (the last content line's
	// terminator); the chomping indicator decides what happens to it.
	return applyChomp(value+"\n", chomp), j, true
}

// applyChomp applies the YAML chomping indicator: "-" strips all trailing
// newlines, "+" keeps them, and the default clips to a single trailing
// newline.
func applyChomp(v, chomp string) string {
	trimmed := strings.TrimRight(v, "\n")
	switch chomp {
	case "-":
		return trimmed
	case "+":
		return v
	default:
		if trimmed == "" {
			return ""
		}
		return trimmed + "\n"
	}
}

// parseIndentedList parses a block list ("key:" followed by indented "- item"
// lines). Returns ok=false when no list items follow.
func parseIndentedList(lines []string, i int) ([]any, int, bool) {
	j := i + 1
	var items []any
	for j < len(lines) {
		line := lines[j]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			j++
			continue
		}
		if indentOf(line) == 0 {
			break
		}
		if trimmed != "-" && !strings.HasPrefix(trimmed, "- ") {
			break
		}
		items = append(items, parseScalar(strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))))
		j++
	}
	if len(items) == 0 {
		return nil, i, false
	}
	return items, j, true
}

// parseIndentedPlain joins indented continuation lines into a single
// space-separated string. Returns ok=false when no indented lines follow.
func parseIndentedPlain(lines []string, i int) (string, int, bool) {
	j := i + 1
	var parts []string
	for j < len(lines) {
		line := lines[j]
		if strings.TrimSpace(line) == "" {
			j++
			continue
		}
		if indentOf(line) == 0 {
			break
		}
		parts = append(parts, strings.TrimSpace(line))
		j++
	}
	if len(parts) == 0 {
		return "", i, false
	}
	return strings.Join(parts, " "), j, true
}

// quotedOrFlow reports whether a raw value starts a quoted or flow-list
// scalar, which never continues on following lines.
func quotedOrFlow(rest string) bool {
	rest = strings.TrimSpace(rest)
	return strings.HasPrefix(rest, `"`) || strings.HasPrefix(rest, "'") || strings.HasPrefix(rest, "[")
}

// parseScalar parses a single-line scalar value: quoted strings (with
// escapes), flow lists, booleans, null, and plain strings (trailing comments
// stripped). Everything else is returned as a string.
func parseScalar(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	switch s[0] {
	case '"':
		if len(s) >= 2 && s[len(s)-1] == '"' {
			return unescapeDouble(s[1 : len(s)-1])
		}
	case '\'':
		if len(s) >= 2 && s[len(s)-1] == '\'' {
			return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
		}
	case '[':
		if strings.HasSuffix(s, "]") {
			return parseFlowList(s[1 : len(s)-1])
		}
	}
	// Plain scalar: a comment starts at " #" (or "\t#").
	for _, sep := range []string{" #", "\t#"} {
		if idx := strings.Index(s, sep); idx != -1 {
			s = strings.TrimSpace(s[:idx])
			break
		}
	}
	switch strings.ToLower(s) {
	case "true":
		return true
	case "false":
		return false
	case "null", "~":
		return ""
	}
	return s
}

// unescapeDouble unescapes a double-quoted YAML scalar.
func unescapeDouble(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '\\', '"', '/', '\'':
			b.WriteByte(s[i])
		case '0':
			b.WriteByte(0)
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// parseFlowList splits a "[a, b, c]" flow list into scalars.
func parseFlowList(s string) []any {
	var out []any
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		out = append(out, parseScalar(item))
	}
	return out
}

// indentOf returns the leading whitespace width of a line (spaces and tabs
// each count as one column).
func indentOf(line string) int {
	n := 0
	for _, c := range line {
		if c == ' ' || c == '\t' {
			n++
		} else {
			break
		}
	}
	return n
}
