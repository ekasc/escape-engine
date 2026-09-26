package resources

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekasc/escape-engine/internal/settings"
)

// withHome isolates ~ from the real user, returning a temp dir as HOME.
func withHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newLoader(cwd string) *Loader {
	return New(cwd, settings.Defaults())
}

func findSkill(t *testing.T, skills []Skill, name string) Skill {
	t.Helper()
	for _, s := range skills {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("skill %q not found in %d skills", name, len(skills))
	return Skill{}
}

func findTemplate(t *testing.T, templates []PromptTemplate, name string) PromptTemplate {
	t.Helper()
	for _, tmpl := range templates {
		if tmpl.Name == name {
			return tmpl
		}
	}
	t.Fatalf("template %q not found in %d templates", name, len(templates))
	return PromptTemplate{}
}

// --- frontmatter parsing ---------------------------------------------------

func TestParseFrontmatterFoldedBlock(t *testing.T) {
	content := "---\nname: demo\nversion: 4\ndescription: >-\n  First line.\n  Second line.\n\n  New paragraph.\nlicense: MIT\n---\n# Body\n\ntext\n"
	fm, body := parseFrontmatter(content)
	if fm["name"] != "demo" {
		t.Errorf("name = %q", fm["name"])
	}
	if fm["version"] != "4" {
		t.Errorf("version = %q", fm["version"])
	}
	want := "First line. Second line.\nNew paragraph."
	if fm["description"] != want {
		t.Errorf("description = %q, want %q", fm["description"], want)
	}
	if fm["license"] != "MIT" {
		t.Errorf("license = %q", fm["license"])
	}
	if body != "# Body\n\ntext" {
		t.Errorf("body = %q", body)
	}
}

func TestParseFrontmatterLiteralBlock(t *testing.T) {
	content := "---\ndescription: |\n  Line one\n  Line two\n\n  Line three\n---\nbody"
	fm, _ := parseFrontmatter(content)
	// Clip chomping keeps exactly one trailing newline.
	want := "Line one\nLine two\n\nLine three\n"
	if fm["description"] != want {
		t.Errorf("description = %q, want %q", fm["description"], want)
	}
}

func TestParseFrontmatterChomping(t *testing.T) {
	cases := []struct {
		header, want string
	}{
		{">-", "a b"},
		{">", "a b\n"},
		{">+", "a b\n"},
		{"|-", "a\nb"},
		{"|", "a\nb\n"},
		{"|+", "a\nb\n"},
	}
	for _, c := range cases {
		content := "---\ndescription: " + c.header + "\n  a\n  b\n---\nx"
		fm, _ := parseFrontmatter(content)
		if fm["description"] != c.want {
			t.Errorf("header %s: description = %q, want %q", c.header, fm["description"], c.want)
		}
	}
}

func TestParseFrontmatterQuotedAndLists(t *testing.T) {
	content := "---\n" +
		"name: \"quoted \\\"name\\\"\"\n" +
		"description: 'single ''quoted'''\n" +
		"allowed-tools: [Read, Write, \"Bash(x)\"]\n" +
		"block-list:\n  - Read\n  - 'Write'\n" +
		"plain: value with # comment\n" +
		"flag: true\n" +
		"other: false\n" +
		"nothing: null\n" +
		"empty: \n" +
		"---\nbody"
	fm, _ := parseFrontmatter(content)
	if fm["name"] != `quoted "name"` {
		t.Errorf("name = %q", fm["name"])
	}
	if fm["description"] != "single 'quoted'" {
		t.Errorf("description = %q", fm["description"])
	}
	flow, ok := fm["allowed-tools"].([]any)
	if !ok || len(flow) != 3 || flow[0] != "Read" || flow[2] != "Bash(x)" {
		t.Errorf("allowed-tools = %#v", fm["allowed-tools"])
	}
	block, ok := fm["block-list"].([]any)
	if !ok || len(block) != 2 || block[0] != "Read" || block[1] != "Write" {
		t.Errorf("block-list = %#v", fm["block-list"])
	}
	if fm["plain"] != "value with" {
		t.Errorf("plain = %q", fm["plain"])
	}
	if fm["flag"] != true || fm["other"] != false {
		t.Errorf("flags = %v %v", fm["flag"], fm["other"])
	}
	if fm["nothing"] != "" || fm["empty"] != "" {
		t.Errorf("nulls = %q %q", fm["nothing"], fm["empty"])
	}
}

func TestParseFrontmatterNoFrontmatter(t *testing.T) {
	content := "# Just a body\nno frontmatter here"
	fm, body := parseFrontmatter(content)
	if len(fm) != 0 {
		t.Errorf("frontmatter should be empty, got %#v", fm)
	}
	if body != content {
		t.Errorf("body should be the full content")
	}
}

func TestParseFrontmatterUnclosed(t *testing.T) {
	content := "---\nname: demo\nno closing delimiter"
	fm, body := parseFrontmatter(content)
	if len(fm) != 0 {
		t.Errorf("frontmatter should be empty when unclosed, got %#v", fm)
	}
	if body != content {
		t.Errorf("body should be the full content when unclosed")
	}
}

func TestParseFrontmatterCRLF(t *testing.T) {
	content := "---\r\nname: demo\r\ndescription: >-\r\n  Folded line.\r\n---\r\nbody"
	fm, body := parseFrontmatter(content)
	if fm["name"] != "demo" {
		t.Errorf("name = %q", fm["name"])
	}
	if fm["description"] != "Folded line." {
		t.Errorf("description = %q", fm["description"])
	}
	if body != "body" {
		t.Errorf("body = %q", body)
	}
}

func TestParseFrontmatterPlainMultiline(t *testing.T) {
	content := "---\ndescription: First line\n  continues here\n  and here\n---\nx"
	fm, _ := parseFrontmatter(content)
	if fm["description"] != "First line continues here and here" {
		t.Errorf("description = %q", fm["description"])
	}
}

// --- context files ---------------------------------------------------------

func TestContextFilesLayering(t *testing.T) {
	home := withHome(t)
	cwd := filepath.Join(home, "proj", "sub")
	global := filepath.Join(home, ".pi", "agent", "AGENTS.md")
	root := filepath.Join(home, "proj", "CLAUDE.md")
	inner := filepath.Join(cwd, "AGENTS.md")
	writeFile(t, global, "global instructions")
	writeFile(t, root, "project root instructions")
	writeFile(t, inner, "subdir instructions")

	got := newLoader(cwd).ContextFiles()
	if !strings.HasPrefix(got, "\n\n<project_context>\n\nProject-specific instructions and guidelines:\n\n") {
		t.Fatalf("bad prefix: %q", got[:min(80, len(got))])
	}
	if !strings.Contains(got, "<project_instructions path=\""+global+"\">\nglobal instructions\n</project_instructions>") {
		t.Errorf("global instructions missing:\n%s", got)
	}
	gi := strings.Index(got, global)
	ri := strings.Index(got, root)
	ii := strings.Index(got, inner)
	if gi == -1 || ri == -1 || ii == -1 || !(gi < ri && ri < ii) {
		t.Errorf("layer order wrong: global=%d root=%d inner=%d", gi, ri, ii)
	}
	if !strings.HasSuffix(got, "</project_context>\n") {
		t.Errorf("bad suffix: %q", got[len(got)-20:])
	}
}

func TestContextFilesOverride(t *testing.T) {
	withHome(t)
	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, "AGENTS.md"), "shadowed")
	writeFile(t, filepath.Join(cwd, "AGENTS.override.md"), "override wins")

	got := newLoader(cwd).ContextFiles()
	if strings.Contains(got, "shadowed") {
		t.Errorf("AGENTS.md must be shadowed by AGENTS.override.md:\n%s", got)
	}
	if !strings.Contains(got, "override wins") {
		t.Errorf("override content missing:\n%s", got)
	}
}

func TestContextFilesNone(t *testing.T) {
	withHome(t)
	cwd := t.TempDir()
	if got := newLoader(cwd).ContextFiles(); got != "" {
		t.Errorf("expected empty context, got %q", got)
	}
}

func TestSystemPromptFiles(t *testing.T) {
	// Project SYSTEM.md replaces and wins over the global one.
	t.Run("project system", func(t *testing.T) {
		home := withHome(t)
		cwd := filepath.Join(home, "proj")
		writeFile(t, filepath.Join(cwd, ".pi", "SYSTEM.md"), "project system")
		writeFile(t, filepath.Join(home, ".pi", "agent", "SYSTEM.md"), "global system")

		content, replace := newLoader(cwd).SystemPrompt()
		if !replace || content != "project system" {
			t.Errorf("project SYSTEM.md: replace=%v content=%q", replace, content)
		}
	})

	// Global SYSTEM.md is used when the project has none.
	t.Run("global system", func(t *testing.T) {
		home := withHome(t)
		cwd := filepath.Join(home, "proj")
		writeFile(t, filepath.Join(home, ".pi", "agent", "SYSTEM.md"), "global system")

		content, replace := newLoader(cwd).SystemPrompt()
		if !replace || content != "global system" {
			t.Errorf("global SYSTEM.md: replace=%v content=%q", replace, content)
		}
	})

	// APPEND_SYSTEM.md appends (replace=false).
	t.Run("append system", func(t *testing.T) {
		home := withHome(t)
		cwd := filepath.Join(home, "proj")
		writeFile(t, filepath.Join(cwd, ".pi", "APPEND_SYSTEM.md"), "append me")
		writeFile(t, filepath.Join(home, ".pi", "agent", "APPEND_SYSTEM.md"), "global append")

		content, replace := newLoader(cwd).SystemPrompt()
		if replace || content != "append me" {
			t.Errorf("project APPEND_SYSTEM.md: replace=%v content=%q", replace, content)
		}

		other := filepath.Join(home, "other")
		content, replace = newLoader(other).SystemPrompt()
		if replace || content != "global append" {
			t.Errorf("global APPEND_SYSTEM.md: replace=%v content=%q", replace, content)
		}
	})

	// No files at all.
	t.Run("none", func(t *testing.T) {
		withHome(t)
		cwd := t.TempDir()
		content, replace := newLoader(cwd).SystemPrompt()
		if replace || content != "" {
			t.Errorf("no files: replace=%v content=%q", replace, content)
		}
	})
}

// --- skills ----------------------------------------------------------------

const skillMD = "---\nname: %s\ndescription: %s\n---\n# %s\n\nbody of %s\n"

func TestSkillsDiscovery(t *testing.T) {
	home := withHome(t)
	cwd := filepath.Join(home, "repo", "pkg")
	// cwd is inside a git repo rooted at repo/.
	writeFile(t, filepath.Join(home, "repo", ".git", "HEAD"), "")

	// ~/.pi/agent/skills: dirs + root .md files.
	writeFile(t, filepath.Join(home, ".pi", "agent", "skills", "alpha", "SKILL.md"),
		"---\nname: alpha\ndescription: Alpha skill.\n---\n# Alpha\n")
	writeFile(t, filepath.Join(home, ".pi", "agent", "skills", "root-skill.md"),
		"---\nname: root-skill\ndescription: Root md skill.\n---\n# Root\n")
	// Recursive discovery.
	writeFile(t, filepath.Join(home, ".pi", "agent", "skills", "nested", "deep", "SKILL.md"),
		"---\nname: deep\ndescription: Deep skill.\n---\n# Deep\n")
	// node_modules and hidden dirs are skipped.
	writeFile(t, filepath.Join(home, ".pi", "agent", "skills", "node_modules", "dep", "SKILL.md"),
		"---\nname: dep\ndescription: Dep skill.\n---\n")
	writeFile(t, filepath.Join(home, ".pi", "agent", "skills", ".hidden", "SKILL.md"),
		"---\nname: hidden\ndescription: Hidden skill.\n---\n")

	// ~/.agents/skills: dirs only, root .md files are ignored.
	writeFile(t, filepath.Join(home, ".agents", "skills", "beta", "SKILL.md"),
		"---\nname: beta\ndescription: Beta skill.\n---\n# Beta\n")
	writeFile(t, filepath.Join(home, ".agents", "skills", "agents-root.md"),
		"---\nname: agents-root\ndescription: Should be ignored.\n---\n")

	// Project .pi/skills.
	writeFile(t, filepath.Join(cwd, ".pi", "skills", "gamma", "SKILL.md"),
		"---\nname: gamma\ndescription: Gamma skill.\n---\n# Gamma\n")
	writeFile(t, filepath.Join(cwd, ".pi", "skills", "proj-root.md"),
		"---\nname: proj-root\ndescription: Project root md skill.\n---\n")

	// .agents/skills walking up: at pkg (cwd) and at the repo root; the
	// user-level ~/.agents/skills is not double-loaded.
	writeFile(t, filepath.Join(cwd, ".agents", "skills", "delta", "SKILL.md"),
		"---\nname: delta\ndescription: Delta skill.\n---\n# Delta\n")
	writeFile(t, filepath.Join(home, "repo", ".agents", "skills", "epsilon", "SKILL.md"),
		"---\nname: epsilon\ndescription: Epsilon skill.\n---\n# Epsilon\n")

	skills := newLoader(cwd).Skills()
	names := map[string]bool{}
	for _, s := range skills {
		names[s.Name] = true
	}
	for _, want := range []string{"alpha", "root-skill", "deep", "beta", "gamma", "proj-root", "delta", "epsilon"} {
		if !names[want] {
			t.Errorf("skill %q not discovered; got %v", want, names)
		}
	}
	for _, unwanted := range []string{"dep", "hidden", "agents-root"} {
		if names[unwanted] {
			t.Errorf("skill %q should not be discovered", unwanted)
		}
	}
}

func TestSkillsFrontmatterValidation(t *testing.T) {
	home := withHome(t)
	cwd := t.TempDir()
	writeFile(t, filepath.Join(home, ".pi", "agent", "skills", "good", "SKILL.md"),
		"---\nname: good\nallowed-tools: [Read, Bash(x)]\ndescription: Good skill.\n---\n# Good\n")
	writeFile(t, filepath.Join(home, ".pi", "agent", "skills", "no-desc", "SKILL.md"),
		"---\nname: no-desc\n---\nbody")
	writeFile(t, filepath.Join(home, ".pi", "agent", "skills", "bad-name", "SKILL.md"),
		"---\nname: Bad_Name\ndescription: Has a bad name, still loads.\n---\nbody")
	writeFile(t, filepath.Join(home, ".pi", "agent", "skills", "no-name", "SKILL.md"),
		"---\ndescription: Uses dir name.\n---\nbody")
	writeFile(t, filepath.Join(home, ".pi", "agent", "skills", "disabled", "SKILL.md"),
		"---\nname: disabled\ndescription: Hidden from the model.\ndisable-model-invocation: true\n---\nbody")

	skills := newLoader(cwd).Skills()
	if len(skills) != 4 {
		t.Fatalf("want 4 skills (no-desc skipped), got %d: %+v", len(skills), skills)
	}
	good := findSkill(t, skills, "good")
	if len(good.AllowedTools) != 2 || good.AllowedTools[0] != "Read" || good.AllowedTools[1] != "Bash(x)" {
		t.Errorf("allowed-tools = %v", good.AllowedTools)
	}
	if good.Body != "# Good" {
		t.Errorf("body = %q", good.Body)
	}
	if good.Path != filepath.Join(home, ".pi", "agent", "skills", "good", "SKILL.md") {
		t.Errorf("path = %q", good.Path)
	}
	// Name falls back to the parent directory name.
	findSkill(t, skills, "no-name")
	// Bad names are loaded with warnings; the description line in the body
	// must not be mistaken for frontmatter.
	findSkill(t, skills, "Bad_Name")
	disabled := findSkill(t, skills, "disabled")
	if !disabled.DisableModelInvocation {
		t.Error("disable-model-invocation should be true")
	}
}

func TestQuietLoaderSuppressesSkillWarnings(t *testing.T) {
	home := withHome(t)
	cwd := t.TempDir()
	writeFile(t, filepath.Join(home, ".pi", "agent", "skills", "dup", "SKILL.md"),
		"---\nname: dup\ndescription: First copy.\n---\n")
	writeFile(t, filepath.Join(home, ".agents", "skills", "dup", "SKILL.md"),
		"---\nname: dup\ndescription: Second copy.\n---\n")

	var output bytes.Buffer
	original := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(original)

	NewQuiet(cwd, settings.Defaults()).Skills()
	if output.Len() != 0 {
		t.Fatalf("quiet loader wrote warnings: %s", output.String())
	}
}

func TestSkillsCollisionFirstWins(t *testing.T) {
	home := withHome(t)
	cwd := t.TempDir()
	writeFile(t, filepath.Join(home, ".pi", "agent", "skills", "dup", "SKILL.md"),
		"---\nname: dup\ndescription: First copy.\n---\n")
	writeFile(t, filepath.Join(home, ".agents", "skills", "dup", "SKILL.md"),
		"---\nname: dup\ndescription: Second copy.\n---\n")

	skills := newLoader(cwd).Skills()
	dup := findSkill(t, skills, "dup")
	if !strings.Contains(dup.Description, "First") {
		t.Errorf("first-found should win, got %q", dup.Description)
	}
}

func TestSkillsSettingsPaths(t *testing.T) {
	home := withHome(t)
	cwd := t.TempDir()
	extraDir := filepath.Join(home, "extra-skills")
	writeFile(t, filepath.Join(extraDir, "from-dir", "SKILL.md"),
		"---\nname: from-dir\ndescription: From settings dir.\n---\n")
	single := filepath.Join(home, "single.md")
	writeFile(t, single, "---\nname: single\ndescription: From settings file.\n---\n")

	s := settings.Defaults()
	s.Skills = []string{extraDir, single}
	skills := New(cwd, s).Skills()
	findSkill(t, skills, "from-dir")
	findSkill(t, skills, "single")
}

func TestSkillsXML(t *testing.T) {
	home := withHome(t)
	cwd := t.TempDir()
	writeFile(t, filepath.Join(home, ".pi", "agent", "skills", "alpha", "SKILL.md"),
		"---\nname: alpha\ndescription: Alpha <skill> & co.\n---\n")
	writeFile(t, filepath.Join(home, ".pi", "agent", "skills", "hidden-skill", "SKILL.md"),
		"---\nname: hidden-skill\ndescription: Hidden.\ndisable-model-invocation: true\n---\n")

	xml := newLoader(cwd).SkillsXML()
	if !strings.Contains(xml, "<available_skills>") || !strings.Contains(xml, "</available_skills>") {
		t.Fatalf("no available_skills wrapper:\n%s", xml)
	}
	if !strings.Contains(xml, "<name>alpha</name>") {
		t.Errorf("alpha missing:\n%s", xml)
	}
	if !strings.Contains(xml, "<description>Alpha &lt;skill&gt; &amp; co.</description>") {
		t.Errorf("description not escaped:\n%s", xml)
	}
	if strings.Contains(xml, "hidden-skill") {
		t.Errorf("disable-model-invocation skill must not appear:\n%s", xml)
	}
	if !strings.Contains(xml, "The following skills provide specialized instructions") {
		t.Errorf("missing usage instructions:\n%s", xml)
	}

	// No visible skills -> empty.
	t.Run("no visible skills", func(t *testing.T) {
		home := withHome(t)
		writeFile(t, filepath.Join(home, ".pi", "agent", "skills", "hidden-skill", "SKILL.md"),
			"---\nname: hidden-skill\ndescription: Hidden.\ndisable-model-invocation: true\n---\n")
		if xml := newLoader(t.TempDir()).SkillsXML(); xml != "" {
			t.Errorf("expected empty XML, got %q", xml)
		}
	})
}

func TestSkillCommand(t *testing.T) {
	home := withHome(t)
	cwd := t.TempDir()
	writeFile(t, filepath.Join(home, ".pi", "agent", "skills", "demo", "SKILL.md"),
		"---\nname: demo\ndescription: Demo.\n---\n# Demo\n\nRun the script:\n```bash\n./scripts/run.sh\n```\n")

	got, ok := newLoader(cwd).SkillCommand("demo", "run tests")
	if !ok {
		t.Fatal("skill not found")
	}
	if !strings.HasPrefix(got, `<skill name="demo" location="`) {
		t.Errorf("bad skill block start:\n%s", got)
	}
	if !strings.Contains(got, "References are relative to "+filepath.Join(home, ".pi", "agent", "skills", "demo")+".\n") {
		t.Errorf("missing references hint:\n%s", got)
	}
	if !strings.Contains(got, "# Demo\n\nRun the script:") {
		t.Errorf("body missing:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n</skill>\n\nUser: run tests") {
		t.Errorf("bad suffix:\n%s", got[len(got)-80:])
	}

	// No args -> no User: line.
	got, ok = newLoader(cwd).SkillCommand("demo", "")
	if !ok || strings.Contains(got, "User:") {
		t.Errorf("no-args expansion wrong: ok=%v\n%s", ok, got)
	}
	// Unknown skill.
	if _, ok := newLoader(cwd).SkillCommand("nope", ""); ok {
		t.Error("unknown skill should return false")
	}
}

// --- templates -------------------------------------------------------------

func TestTemplatesDiscovery(t *testing.T) {
	home := withHome(t)
	cwd := filepath.Join(home, "proj")
	writeFile(t, filepath.Join(home, ".pi", "agent", "prompts", "global.md"),
		"---\ndescription: Global template\n---\nGlobal body $1")
	// Subdirectories are not scanned.
	writeFile(t, filepath.Join(home, ".pi", "agent", "prompts", "sub", "nested.md"),
		"---\ndescription: Nested\n---\nbody")
	writeFile(t, filepath.Join(cwd, ".pi", "prompts", "review.md"),
		"---\ndescription: Review staged changes\nargument-hint: \"<PR-URL>\"\n---\nReview body.")
	// No frontmatter: description falls back to the first body line.
	writeFile(t, filepath.Join(cwd, ".pi", "prompts", "bare.md"),
		"First non-empty line is the description.\n\nMore body.")

	templates := newLoader(cwd).Templates()
	global := findTemplate(t, templates, "global")
	if global.Description != "Global template" || global.Body != "Global body $1" {
		t.Errorf("global = %+v", global)
	}
	review := findTemplate(t, templates, "review")
	if review.Description != "Review staged changes" || review.ArgumentHint != "<PR-URL>" {
		t.Errorf("review = %+v", review)
	}
	bare := findTemplate(t, templates, "bare")
	if bare.Description != "First non-empty line is the description." {
		t.Errorf("bare description = %q", bare.Description)
	}
	for _, tpl := range templates {
		if tpl.Name == "nested" {
			t.Error("nested template must not be discovered (non-recursive)")
		}
	}
}

func TestTemplatesSettingsPaths(t *testing.T) {
	home := withHome(t)
	cwd := t.TempDir()
	dir := filepath.Join(home, "extra-prompts")
	writeFile(t, filepath.Join(dir, "a.md"), "---\ndescription: A\n---\nbody")
	writeFile(t, filepath.Join(dir, "sub", "b.md"), "---\ndescription: B\n---\nbody")
	single := filepath.Join(home, "single.md")
	writeFile(t, single, "---\ndescription: Single\n---\nbody")

	s := settings.Defaults()
	s.Prompts = []string{dir, single}
	templates := New(cwd, s).Templates()
	findTemplate(t, templates, "a")
	findTemplate(t, templates, "b") // settings dirs are recursive
	findTemplate(t, templates, "single")
}

func TestExpandTemplateArgs(t *testing.T) {
	home := withHome(t)
	cwd := t.TempDir()
	body := "create $1 with $2 and ${3:-default}; all: $@; from2: ${@:2}; two-from2: ${@:2:2}; def: ${1:-fallback}; empty-def: ${2:-fallback}; args-def: ${@:-allfallback}; ARGS: $ARGUMENTS; zero: $0; "
	writeFile(t, filepath.Join(home, ".pi", "agent", "prompts", "t.md"), "---\ndescription: T\n---\n"+body)

	got, ok := newLoader(cwd).ExpandTemplate("t", `Button "click handler"`)
	if !ok {
		t.Fatal("template not found")
	}
	want := "create Button with click handler and default; all: Button click handler; from2: click handler; two-from2: click handler; def: Button; empty-def: click handler; args-def: Button click handler; ARGS: Button click handler; zero: ;"
	if got != want {
		t.Errorf("expansion mismatch:\n got %q\nwant %q", got, want)
	}

	// ${@:N} past the end and ${@:N:L} clipping; ${@:0} is treated as ${@:1}.
	writeFile(t, filepath.Join(home, ".pi", "agent", "prompts", "slice.md"), "---\ndescription: Slice\n---\n[${@:5}] [${@:1:2}] [${@:0}] [${@:2:5}]")
	got, _ = newLoader(cwd).ExpandTemplate("slice", "a b")
	want = "[] [a b] [a b] [b]"
	if got != want {
		t.Errorf("slice expansion: got %q want %q", got, want)
	}

	// Unknown template.
	if _, ok := newLoader(cwd).ExpandTemplate("missing", ""); ok {
		t.Error("unknown template should return false")
	}
}

func TestGetCommands(t *testing.T) {
	home := withHome(t)
	cwd := t.TempDir()
	writeFile(t, filepath.Join(home, ".pi", "agent", "skills", "alpha", "SKILL.md"),
		"---\nname: alpha\ndescription: Alpha skill.\n---\n")
	writeFile(t, filepath.Join(home, ".pi", "agent", "prompts", "review.md"),
		"---\ndescription: Review changes\nargument-hint: \"<X>\"\n---\nbody")

	cmds := newLoader(cwd).GetCommands()
	if len(cmds) != 2 {
		t.Fatalf("want 2 commands, got %d: %+v", len(cmds), cmds)
	}
	var skillCmd, promptCmd Command
	for _, c := range cmds {
		switch c.Source {
		case "skill":
			skillCmd = c
		case "prompt":
			promptCmd = c
		}
	}
	if skillCmd.Name != "skill:alpha" || skillCmd.Description != "Alpha skill." || skillCmd.Location != "user" || skillCmd.Path == "" {
		t.Errorf("skill command = %+v", skillCmd)
	}
	if promptCmd.Name != "review" || promptCmd.Source != "prompt" || promptCmd.ArgumentHint != "<X>" || promptCmd.Location != "user" {
		t.Errorf("prompt command = %+v", promptCmd)
	}
}

// TestRealWorldSkills sanity-checks the frontmatter parser against the
// installed pi skills. Skipped unless PI_GO_REAL_SKILLS is set, so the suite
// stays hermetic.
func TestRealWorldSkills(t *testing.T) {
	if os.Getenv("PI_GO_REAL_SKILLS") == "" {
		t.Skip("set PI_GO_REAL_SKILLS=1 to run against real skill dirs")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cwd := t.TempDir()
	l := newLoader(cwd)
	skills := l.Skills()
	if len(skills) == 0 {
		t.Fatal("no skills discovered from real dirs")
	}
	names := map[string]bool{}
	for _, s := range skills {
		names[s.Name] = true
		if s.Description == "" {
			t.Errorf("skill %s has empty description", s.Name)
		}
	}
	for _, want := range []string{"amazon-bedrock", "apple-design", "humanizer", "shadcn", "ui-ux-pro-max"} {
		if !names[want] {
			t.Errorf("expected real skill %q, got %d skills", want, len(skills))
		}
	}
	t.Logf("discovered %d real skills", len(skills))
	_ = home
}
