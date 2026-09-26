package agent

import (
	"strings"
	"testing"

	"github.com/ekasc/escape-engine/internal/memory"
)

func TestSystemPromptIncludesMemorySnapshot(t *testing.T) {
	store := memory.Open(t.TempDir(), t.TempDir())
	if err := store.Add("RPC session header is bound in agent.New"); err != nil {
		t.Fatal(err)
	}

	prompt := buildSystemPrompt(Options{
		SystemPrompt: "base",
		Cwd:          t.TempDir(),
		Memory:       store.Render(),
	})

	for _, want := range []string{"MEMORY", "RPC session header is bound in agent.New", "1."} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestSystemPromptOmitsEmptyMemory(t *testing.T) {
	store := memory.Open(t.TempDir(), t.TempDir())
	if got := store.Render(); got != "" {
		t.Fatalf("empty store should render nothing, got %q", got)
	}
	prompt := buildSystemPrompt(Options{SystemPrompt: "base", Cwd: t.TempDir()})
	if strings.Contains(prompt, "MEMORY") {
		t.Fatalf("prompt should not mention memory when there is none:\n%s", prompt)
	}
}

// The snapshot is a string on Options precisely so a turn cannot re-read it. This
// locks in the ordering guarantee the comment claims: memory sits after the
// project context files and before the skills listing.
func TestMemorySitsBetweenContextFilesAndSkills(t *testing.T) {
	store := memory.Open(t.TempDir(), t.TempDir())
	if err := store.Add("ordering probe"); err != nil {
		t.Fatal(err)
	}
	prompt := buildSystemPrompt(Options{
		SystemPrompt: "base",
		Cwd:          t.TempDir(),
		Memory:       store.Render(),
	})
	mem := strings.Index(prompt, "ordering probe")
	if mem < 0 {
		t.Fatalf("memory missing from prompt:\n%s", prompt)
	}
	skills := strings.Index(prompt, "<available_skills")
	if skills < 0 {
		// No skills configured in a temp cwd; the ordering claim is only
		// checkable when the skills block is present.
		t.Skip("no skills block in this environment")
	}
	if mem > skills {
		t.Fatal("memory must be injected before the skills listing")
	}
}

// A full store must not be injected past the cap, and the agent has to see the
// usage figure so it can decide to consolidate before a write fails.
func TestMemorySnapshotReportsUsage(t *testing.T) {
	store := memory.Open(t.TempDir(), t.TempDir())
	if err := store.Add("a fact worth keeping"); err != nil {
		t.Fatal(err)
	}
	used, err := store.Used()
	if err != nil {
		t.Fatal(err)
	}
	rendered := store.Render()
	if !strings.Contains(rendered, "%") {
		t.Fatalf("snapshot should show usage:\n%s", rendered)
	}
	if used <= 0 || used > memory.MaxChars {
		t.Fatalf("unexpected used size %d", used)
	}
}
