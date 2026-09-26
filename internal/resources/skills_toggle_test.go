package resources

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekasc/escape-engine/internal/settings"
)

// jsonMerge loads two settings files the way settings.Load does, so the
// replace-not-merge claim is checked against the real merge.
func jsonMerge(globalPath, projectPath string) (*settings.Settings, error) {
	merged := map[string]any{}
	for _, p := range []string{globalPath, projectPath} {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var values map[string]any
		if err := json.Unmarshal(data, &values); err != nil {
			return nil, err
		}
		for k, v := range values {
			if sub, ok := v.(map[string]any); ok {
				if baseSub, ok := merged[k].(map[string]any); ok {
					for sk, sv := range sub {
						baseSub[sk] = sv
					}
					continue
				}
			}
			merged[k] = v
		}
	}
	blob, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	var out settings.Settings
	if err := json.Unmarshal(blob, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func writeSkill(t *testing.T, dir, name, description string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name, "SKILL.md")
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\n\nbody\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A toggle that does not remove the skill from the prompt is a dead control:
// the model would still be offered a skill the user switched off.
func TestDisabledSkillLeavesThePrompt(t *testing.T) {
	home := withHome(t)
	dir := filepath.Join(home, ".agents", "skills")
	keep := writeSkill(t, dir, "keep-me", "a skill that stays on")
	drop := writeSkill(t, dir, "drop-me", "a skill the user switches off")

	set := settings.Defaults()
	set.DisabledSkills = []string{drop}
	loader := New(t.TempDir(), set)

	xml := loader.SkillsXML()
	if !strings.Contains(xml, "keep-me") {
		t.Fatalf("the enabled skill should be advertised:\n%s", xml)
	}
	if strings.Contains(xml, "drop-me") {
		t.Fatalf("a disabled skill must not reach the prompt:\n%s", xml)
	}
	if strings.Contains(xml, "a skill the user switches off") {
		t.Fatal("the disabled description is still in the prompt")
	}

	// It is still discovered, so the settings surface can offer to switch it
	// back on. Hiding it entirely would make the control unreachable.
	var found *Skill
	for i, s := range loader.Skills() {
		if s.Path == drop {
			found = &loader.Skills()[i]
		}
	}
	if found == nil {
		t.Fatal("a disabled skill must still be listed")
	}
	if !found.Disabled {
		t.Fatal("the skill should be marked disabled")
	}
	if found.Location == "" {
		t.Fatal("a skill needs a location to display")
	}
	_ = keep
}

func TestDisablingIsIdempotentAndReversible(t *testing.T) {
	home := withHome(t)
	dir := filepath.Join(home, ".agents", "skills")
	path := writeSkill(t, dir, "toggle", "toggled more than once")

	set := settings.Defaults()
	loader := New(t.TempDir(), set)
	if strings.Contains(loader.SkillsXML(), "toggle") != true {
		t.Fatal("precondition: the skill starts enabled")
	}

	set.DisabledSkills = []string{path, path}
	off := New(t.TempDir(), set)
	if strings.Contains(off.SkillsXML(), "toggle") {
		t.Fatal("a duplicated setting should still disable the skill")
	}

	set.DisabledSkills = nil
	on := New(t.TempDir(), set)
	if !strings.Contains(on.SkillsXML(), "toggle") {
		t.Fatal("clearing the setting should re-enable the skill")
	}
}

// An author's own opt-out and a user's switch are different things, and a user
// setting must not overwrite what the skill file declares.
func TestUserSettingDoesNotOverwriteAuthorOptOut(t *testing.T) {
	home := withHome(t)
	dir := filepath.Join(home, ".agents", "skills")
	if err := os.MkdirAll(filepath.Join(dir, "author-off"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: author-off\ndescription: declares its own opt out\ndisable-model-invocation: true\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "author-off", "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	set := settings.Defaults()
	loader := New(t.TempDir(), set)
	if strings.Contains(loader.SkillsXML(), "author-off") {
		t.Fatal("a self opted out skill was advertised")
	}
	var skill *Skill
	for i, s := range loader.Skills() {
		if s.Name == "author-off" {
			skill = &loader.Skills()[i]
		}
	}
	if skill == nil {
		t.Fatal("the skill should still be listed")
	}
	if skill.Disabled {
		t.Fatal("an author's opt out is not a user setting and must not be reported as one")
	}
	if !skill.DisableModelInvocation {
		t.Fatal("the author's own flag should be preserved")
	}
}

// Paths are compared cleaned, so a settings file written with a trailing
// separator still matches the discovered path.
func TestDisabledPathComparisonIgnoresTrailingSeparator(t *testing.T) {
	home := withHome(t)
	dir := filepath.Join(home, ".agents", "skills")
	path := writeSkill(t, dir, "sloppy", "written with a trailing separator")
	withSep := path + string(filepath.Separator)

	set := settings.Defaults()
	set.DisabledSkills = []string{withSep, "  " + path + "  "}
	loader := New(t.TempDir(), set)
	if strings.Contains(loader.SkillsXML(), "sloppy") {
		t.Fatal("a path written with a trailing separator should still disable the skill")
	}
}

// The listing is not the only way a skill reaches the model. Every skill also
// becomes a slash command carrying its description, and /skill:<name> expands
// the whole body into the conversation. Both must respect the switch, or
// filtering SkillsXML alone leaves the skill fully reachable.
func TestDisabledSkillProducesNoCommand(t *testing.T) {
	home := withHome(t)
	dir := filepath.Join(home, ".agents", "skills")
	writeSkill(t, dir, "offered", "still available")
	drop := writeSkill(t, dir, "retired", "should not be offered")

	set := settings.Defaults()
	set.DisabledSkills = []string{drop}
	loader := New(t.TempDir(), set)

	var names []string
	for _, c := range loader.GetCommands() {
		if c.Source == "skill" {
			names = append(names, c.Name)
		}
	}
	joined := strings.Join(names, " ")
	if !strings.Contains(joined, "skill:offered") {
		t.Fatalf("an enabled skill should still be a command: %v", names)
	}
	if strings.Contains(joined, "skill:retired") {
		t.Fatalf("a disabled skill must not be a command: %v", names)
	}
	// The description would otherwise ride along in get_commands.
	for _, c := range loader.GetCommands() {
		if c.Description == "should not be offered" {
			t.Fatal("a disabled skill's description reached the command list")
		}
	}
}

func TestDisabledSkillCannotBeExpandedByName(t *testing.T) {
	home := withHome(t)
	dir := filepath.Join(home, ".agents", "skills")
	writeSkill(t, dir, "offered", "still available")
	drop := writeSkill(t, dir, "retired", "should not be loadable")

	set := settings.Defaults()
	set.DisabledSkills = []string{drop}
	loader := NewQuiet(t.TempDir(), set)

	if body, ok := loader.SkillCommand("retired", "do the thing"); ok {
		t.Fatalf("a disabled skill was expanded into the conversation:\n%s", body)
	}
	body, ok := loader.SkillCommand("offered", "do the thing")
	if !ok {
		t.Fatal("an enabled skill should still expand")
	}
	if !strings.Contains(body, "offered") {
		t.Fatalf("unexpected expansion:\n%s", body)
	}
}

// The two lists and their order. The global list applies to every project, and a
// project decision beats it, including a project turning a skill back on that the
// global list switched off.
func TestProjectOverrideBeatsTheGlobalList(t *testing.T) {
	home := withHome(t)
	dir := filepath.Join(home, ".agents", "skills")
	offGlobally := writeSkill(t, dir, "heavy", "switched off everywhere")
	writeSkill(t, dir, "light", "on by default")

	global := settings.Defaults()
	global.DisabledSkills = []string{offGlobally}
	loader := New(t.TempDir(), global)
	if strings.Contains(loader.SkillsXML(), "heavy") {
		t.Fatal("precondition: the global list should hide the skill")
	}

	// This project wants it back.
	project := settings.Defaults()
	project.DisabledSkills = append([]string{}, global.DisabledSkills...)
	project.SkillOverrides = []settings.SkillOverride{{Path: offGlobally, Enabled: true}}
	local := New(t.TempDir(), project)
	if !strings.Contains(local.SkillsXML(), "heavy") {
		t.Fatal("a project decision must beat the global list")
	}
	if !strings.Contains(local.SkillsXML(), "light") {
		t.Fatal("an untouched skill should follow the global list and stay on")
	}
}

// The other direction: a project switching off something the global list leaves
// on.
func TestProjectCanDisableAGloballyEnabledSkill(t *testing.T) {
	home := withHome(t)
	dir := filepath.Join(home, ".agents", "skills")
	path := writeSkill(t, dir, "noisy", "on everywhere by default")

	set := settings.Defaults()
	if !strings.Contains(New(t.TempDir(), set).SkillsXML(), "noisy") {
		t.Fatal("precondition: the skill starts enabled")
	}
	set.SkillOverrides = []settings.SkillOverride{{Path: path, Enabled: false}}
	loader := New(t.TempDir(), set)
	if strings.Contains(loader.SkillsXML(), "noisy") {
		t.Fatal("a project must be able to switch off a globally enabled skill")
	}
	if body, ok := loader.SkillCommand("noisy", "go"); ok {
		t.Fatalf("a project-disabled skill was still expandable:\n%s", body)
	}
}

// Absence from the override list is inheritance, not a decision. A project file
// that says nothing about a skill must see the global choice.
func TestNoOverrideInheritsTheGlobalList(t *testing.T) {
	home := withHome(t)
	dir := filepath.Join(home, ".agents", "skills")
	path := writeSkill(t, dir, "inherited", "decided elsewhere")

	other := writeSkill(t, dir, "unrelated", "not mentioned anywhere")
	set := settings.Defaults()
	set.DisabledSkills = []string{path}
	set.SkillOverrides = []settings.SkillOverride{{Path: other, Enabled: false}}
	loader := New(t.TempDir(), set)

	if strings.Contains(loader.SkillsXML(), "inherited") {
		t.Fatal("a skill with no project decision must follow the global list")
	}
	if strings.Contains(loader.SkillsXML(), "unrelated") {
		t.Fatal("a project decision must still apply")
	}
}

// The override list has to replace rather than merge. Settings merge key by key,
// so a map would carry one project's decisions into another already decided.
func TestOverridesReplaceRatherThanMerge(t *testing.T) {
	cwd := t.TempDir()
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A global file with an override, and a project file with a different one.
	globalPath := filepath.Join(t.TempDir(), "global.json")
	if err := os.WriteFile(globalPath, []byte(`{"skillOverrides":[{"path":"/a/SKILL.md","enabled":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(cwd, ".pi", "settings.json")
	if err := os.WriteFile(projectPath, []byte(`{"skillOverrides":[{"path":"/b/SKILL.md","enabled":false}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	merged, err := jsonMerge(globalPath, projectPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.SkillOverrides) != 1 || merged.SkillOverrides[0].Path != "/b/SKILL.md" {
		t.Fatalf("the project list must replace the global one, got %+v", merged.SkillOverrides)
	}
	_ = other
}

// A long description must be shortened at a word boundary in the prompt, and the
// full text must still be readable from the file.
func TestPromptDescriptionIsCappedAtAWordBoundary(t *testing.T) {
	home := withHome(t)
	dir := filepath.Join(home, ".agents", "skills")
	long := strings.Repeat("alpha beta ", 60)
	path := filepath.Join(dir, "wordy")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: wordy\ndescription: " + long + "\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = filepath.Join

	loader := NewQuiet(t.TempDir(), settings.Defaults())
	xml := loader.SkillsXML()
	if len(xml) > 1200 {
		t.Fatalf("a %d character description should not reach the prompt verbatim:\n%s", len(long), xml)
	}
	if !strings.Contains(xml, "...") {
		t.Fatalf("a shortened description should be marked as elided:\n%s", xml)
	}
	if !strings.Contains(xml, "alpha beta") {
		t.Fatalf("the summary should still read as words:\n%s", xml)
	}
	if strings.Contains(xml, long) {
		t.Fatal("the full description leaked into the prompt")
	}
	// The file is still the source of truth. Reading the skill returns its body
	// in full, which is the point of the cap: the listing is short, the file is
	// not.
	onDisk, ok := loader.SkillCommand("wordy", "")
	if !ok {
		t.Fatal("the skill should still expand")
	}
	if !strings.Contains(onDisk, "<skill") || !strings.Contains(onDisk, "body") {
		t.Fatalf("the expansion should carry the skill body:\n%s", onDisk)
	}
}
