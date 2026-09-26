package resources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekasc/escape-engine/internal/settings"
)

// One test for every route a skill could take into the conversation, because
// fixing only the obvious one leaves the skill fully reachable.
func TestDisabledSkillHasNoRouteIntoTheModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ESCAPE_SESSIONS_DIR", filepath.Join(home, "sessions"))
	dir := filepath.Join(home, ".agents", "skills")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := "CANARY"
	body := "BODY-" + marker
	desc := "DESC-" + marker
	skillDir := filepath.Join(dir, "retired")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	front := "---\nname: retired\ndescription: " + desc + "\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(front), 0o600); err != nil {
		t.Fatal(err)
	}

	set := settings.Defaults()
	set.DisabledSkills = []string{filepath.Join(skillDir, "SKILL.md")}
	loader := NewQuiet(t.TempDir(), set)

	routes := map[string]string{
		"system prompt":      loader.SkillsXML(),
		"skill command list": sprintCommands(loader.GetCommands()),
	}
	if expanded, ok := loader.SkillCommand("retired", "do it"); ok {
		routes["expansion by name"] = expanded
	}
	for where, content := range routes {
		if strings.Contains(content, marker) {
			t.Fatalf("a disabled skill reached the model via %s:\n%s", where, content)
		}
	}
	if _, ok := loader.SkillCommand("retired", ""); ok {
		t.Fatal("the expansion route did not report failure")
	}
}

func sprintCommands(cmds []Command) string {
	var b strings.Builder
	for _, c := range cmds {
		b.WriteString(c.Name)
		b.WriteString(" ")
		b.WriteString(c.Description)
		b.WriteString("\n")
	}
	return b.String()
}
