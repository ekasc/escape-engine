package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestDefaultsWhenNoFiles(t *testing.T) {
	withHome(t)
	cwd := t.TempDir()

	s, err := Load(cwd)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if s.Compaction.Enabled != true || s.Compaction.ReserveTokens != 16384 || s.Compaction.KeepRecentTokens != 20000 {
		t.Errorf("compaction defaults wrong: %+v", s.Compaction)
	}
	if s.BranchSummary.ReserveTokens != 16384 || s.BranchSummary.SkipPrompt != false {
		t.Errorf("branchSummary defaults wrong: %+v", s.BranchSummary)
	}
	if s.Retry.Enabled != true || s.Retry.MaxRetries != 3 || s.Retry.BaseDelayMs != 2000 {
		t.Errorf("retry defaults wrong: %+v", s.Retry)
	}
	if s.SteeringMode != "one-at-a-time" || s.FollowUpMode != "one-at-a-time" {
		t.Errorf("steering/followup defaults wrong: %q %q", s.SteeringMode, s.FollowUpMode)
	}
	if !s.EnableSkillCommands {
		t.Error("enableSkillCommands default should be true")
	}
	if s.DefaultProjectTrust != "ask" {
		t.Errorf("defaultProjectTrust default wrong: %q", s.DefaultProjectTrust)
	}
	if s.ApprovalMode != "auto" {
		t.Errorf("approvalMode default wrong: %q", s.ApprovalMode)
	}
	if s.DefaultProvider != "" || s.DefaultModel != "" || s.SessionDir != "" {
		t.Errorf("optional fields should be empty, got %+v", s)
	}
}

func TestMissingFilesAreNotErrors(t *testing.T) {
	withHome(t)
	cwd := t.TempDir()

	// No ~/.escape/settings.json, no <cwd>/.escape/settings.json.
	s, err := Load(cwd)
	if err != nil {
		t.Fatalf("Load with no settings files: %v", err)
	}
	if s.DefaultModel != "" || s.Retry.MaxRetries != 3 {
		t.Errorf("expected defaults, got %+v", s)
	}
}

func TestGlobalSettings(t *testing.T) {
	home := withHome(t)
	cwd := t.TempDir()

	writeFile(t, filepath.Join(home, ".escape", "settings.json"), `{
		"defaultProvider": "anthropic",
		"defaultModel": "claude-sonnet-4-20250514",
		"compaction": { "enabled": false },
		"sessionDir": "~/.escape/sessions"
	}`)

	s, err := Load(cwd)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.DefaultProvider != "anthropic" || s.DefaultModel != "claude-sonnet-4-20250514" {
		t.Errorf("global provider/model not applied: %+v", s)
	}
	if s.Compaction.Enabled != false {
		t.Error("global compaction.enabled=false not applied")
	}
	if s.Compaction.ReserveTokens != 16384 {
		t.Errorf("unset compaction.reserveTokens should stay default, got %d", s.Compaction.ReserveTokens)
	}
	if s.SessionDir != "~/.escape/sessions" {
		t.Errorf("sessionDir not applied: %q", s.SessionDir)
	}
	if s.Retry.MaxRetries != 3 {
		t.Errorf("unset retry should stay default, got %+v", s.Retry)
	}
}

func TestSetGlobalProviderPreservesSettings(t *testing.T) {
	home := withHome(t)
	cwd := t.TempDir()
	path := filepath.Join(home, ".escape", "settings.json")
	writeFile(t, path, `{"defaultModel":"configured-model","compaction":{"enabled":false}}`)

	if err := SetGlobalProvider("opencode-zen"); err != nil {
		t.Fatalf("SetGlobalProvider: %v", err)
	}
	s, err := Load(cwd)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.DefaultProvider != "opencode-zen" || s.DefaultModel != "configured-model" || s.Compaction.Enabled {
		t.Fatalf("settings after provider selection = %+v", s)
	}
}

func TestSetGlobalProviderCreatesSettings(t *testing.T) {
	home := withHome(t)
	if err := SetGlobalProvider("codex"); err != nil {
		t.Fatalf("SetGlobalProvider: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".escape", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"defaultProvider": "codex"`) {
		t.Fatalf("settings = %s", data)
	}
}

func TestProjectOverridesGlobal(t *testing.T) {
	home := withHome(t)
	cwd := t.TempDir()

	writeFile(t, filepath.Join(home, ".escape", "settings.json"), `{
		"defaultProvider": "anthropic",
		"defaultModel": "claude-sonnet-4-20250514",
		"defaultThinkingLevel": "high",
		"defaultTools": ["bash", "edit"],
		"steeringMode": "all"
	}`)
	writeFile(t, filepath.Join(cwd, ".escape", "settings.json"), `{
		"defaultModel": "gpt-5.6",
		"steeringMode": "one-at-a-time",
		"defaultTools": ["bash"],
		"approvalMode": "ask"
	}`)

	s, err := Load(cwd)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.DefaultProvider != "anthropic" {
		t.Errorf("project should not override defaultProvider: %q", s.DefaultProvider)
	}
	if s.DefaultModel != "gpt-5.6" {
		t.Errorf("project should override defaultModel, got %q", s.DefaultModel)
	}
	if s.DefaultThinkingLevel != "high" {
		t.Errorf("global defaultThinkingLevel should survive, got %q", s.DefaultThinkingLevel)
	}
	if s.SteeringMode != "one-at-a-time" {
		t.Errorf("project should override steeringMode, got %q", s.SteeringMode)
	}
	if len(s.DefaultTools) != 1 || s.DefaultTools[0] != "bash" {
		t.Errorf("project defaultTools should replace global array, got %v", s.DefaultTools)
	}
	if s.ApprovalMode != "ask" {
		t.Errorf("project should override approvalMode, got %q", s.ApprovalMode)
	}
}

func TestNestedMergeProjectPartial(t *testing.T) {
	home := withHome(t)
	cwd := t.TempDir()

	writeFile(t, filepath.Join(home, ".escape", "settings.json"), `{
		"compaction": { "enabled": true, "reserveTokens": 16384, "keepRecentTokens": 20000 },
		"retry": { "enabled": true, "maxRetries": 3, "baseDelayMs": 2000 }
	}`)
	// Project only sets one nested field per group; the rest must survive.
	writeFile(t, filepath.Join(cwd, ".escape", "settings.json"), `{
		"compaction": { "reserveTokens": 8192 },
		"retry": { "maxRetries": 1 }
	}`)

	s, err := Load(cwd)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Compaction.Enabled != true || s.Compaction.ReserveTokens != 8192 || s.Compaction.KeepRecentTokens != 20000 {
		t.Errorf("nested compaction merge wrong: %+v", s.Compaction)
	}
	if s.Retry.Enabled != true || s.Retry.MaxRetries != 1 || s.Retry.BaseDelayMs != 2000 {
		t.Errorf("nested retry merge wrong: %+v", s.Retry)
	}
}

func TestProjectOnlySettings(t *testing.T) {
	withHome(t)
	cwd := t.TempDir()

	writeFile(t, filepath.Join(cwd, ".escape", "settings.json"), `{
		"defaultModel": "deepseek-v4-flash",
		"defaultProjectTrust": "always",
		"skills": ["~/.escape/skills"],
		"enableSkillCommands": false
	}`)

	s, err := Load(cwd)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.DefaultModel != "deepseek-v4-flash" {
		t.Errorf("project defaultModel not applied: %q", s.DefaultModel)
	}
	if s.DefaultProjectTrust != "always" {
		t.Errorf("project defaultProjectTrust not applied: %q", s.DefaultProjectTrust)
	}
	if len(s.Skills) != 1 || s.Skills[0] != "~/.escape/skills" {
		t.Errorf("project skills not applied: %v", s.Skills)
	}
	if s.EnableSkillCommands {
		t.Error("project enableSkillCommands=false not applied")
	}
}

func TestEmptyProjectFileIgnored(t *testing.T) {
	withHome(t)
	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, ".escape", "settings.json"), "   \n  ")

	s, err := Load(cwd)
	if err != nil {
		t.Fatalf("Load with empty project file: %v", err)
	}
	if s.SteeringMode != "one-at-a-time" {
		t.Errorf("defaults expected, got %+v", s)
	}
}

func TestMalformedJSONErrors(t *testing.T) {
	home := withHome(t)
	cwd := t.TempDir()

	writeFile(t, filepath.Join(home, ".escape", "settings.json"), `{"defaultProvider": `)
	if _, err := Load(cwd); err == nil {
		t.Error("expected error for malformed global settings, got nil")
	}

	writeFile(t, filepath.Join(home, ".escape", "settings.json"), `{}`)
	writeFile(t, filepath.Join(cwd, ".escape", "settings.json"), `not json`)
	if _, err := Load(cwd); err == nil {
		t.Error("expected error for malformed project settings, got nil")
	}
}

func TestFieldCaseInsensitiveMatching(t *testing.T) {
	home := withHome(t)
	cwd := t.TempDir()

	// Non-canonical casing must still match (encoding/json is
	// case-insensitive; the merge lowercases keys).
	writeFile(t, filepath.Join(home, ".escape", "settings.json"), `{
		"DefaultModel": "claude-sonnet-4-20250514",
		"Compaction": { "Enabled": false, "KeepRecentTokens": 30000 }
	}`)
	writeFile(t, filepath.Join(cwd, ".escape", "settings.json"), `{
		"COMPACTION": { "RESERVETOKENS": 4096 }
	}`)

	s, err := Load(cwd)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.DefaultModel != "claude-sonnet-4-20250514" {
		t.Errorf("case-insensitive field match failed: %q", s.DefaultModel)
	}
	if s.Compaction.Enabled != false || s.Compaction.ReserveTokens != 4096 || s.Compaction.KeepRecentTokens != 30000 {
		t.Errorf("case-insensitive nested merge failed: %+v", s.Compaction)
	}
}

func TestGlobalDir(t *testing.T) {
	home := withHome(t)
	got := GlobalDir()
	want := filepath.Join(home, ".escape")
	if got != want {
		t.Errorf("GlobalDir() = %q, want %q", got, want)
	}
}

func TestProjectDir(t *testing.T) {
	withHome(t)
	base := t.TempDir()
	mkdir := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// No .escape anywhere: fall back to <cwd>/.escape.
	cwd := filepath.Join(base, "a", "b")
	mkdir(cwd)
	if got, want := ProjectDir(cwd), filepath.Join(cwd, ".escape"); got != want {
		t.Errorf("ProjectDir(%q) = %q, want %q", cwd, got, want)
	}

	// Nearest .escape wins.
	piA := filepath.Join(base, "a", ".escape")
	mkdir(piA)
	if got, want := ProjectDir(cwd), piA; got != want {
		t.Errorf("ProjectDir(%q) = %q, want %q", cwd, got, want)
	}

	// Walk-up finds an ancestor .escape.
	piBase := filepath.Join(base, ".escape")
	mkdir(piBase)
	deep := filepath.Join(base, "x", "y", "z")
	mkdir(deep)
	if got, want := ProjectDir(deep), piBase; got != want {
		t.Errorf("ProjectDir(%q) = %q, want %q", deep, got, want)
	}

	// The walk stops at the git root (here a .git file, as in worktrees): a
	// .escape above it must not win.
	gitRoot := filepath.Join(base, "repo")
	gitCwd := filepath.Join(gitRoot, "src", "pkg")
	mkdir(gitRoot)
	mkdir(gitCwd)
	if err := os.WriteFile(filepath.Join(gitRoot, ".git"), []byte("gitdir: ../.git/worktrees/repo"), 0o644); err != nil {
		t.Fatal(err)
	}
	// base/.escape (piBase) sits above the git root; the walk must not reach it.
	if got, want := ProjectDir(gitCwd), filepath.Join(gitCwd, ".escape"); got != want {
		t.Errorf("ProjectDir(%q) = %q, want %q (walk must stop at git root)", gitCwd, got, want)
	}

	// An .escape at the git root itself is still found.
	piGit := filepath.Join(gitRoot, ".escape")
	mkdir(piGit)
	if got, want := ProjectDir(gitCwd), piGit; got != want {
		t.Errorf("ProjectDir(%q) = %q, want %q", gitCwd, got, want)
	}
}

func TestLoadUsesCwdProjectFile(t *testing.T) {
	withHome(t)
	base := t.TempDir()
	cwd := filepath.Join(base, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cwd, ".escape", "settings.json"), `{"defaultModel": "minimax-m3"}`)

	s, err := Load(cwd)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.DefaultModel != "minimax-m3" {
		t.Errorf("project settings not loaded from cwd: %q", s.DefaultModel)
	}
	// A settings file in a parent's .pi must NOT leak into Load.
	writeFile(t, filepath.Join(base, ".pi", "settings.json"), `{"defaultModel": "should-not-win"}`)
	s, err = Load(cwd)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.DefaultModel != "minimax-m3" {
		t.Errorf("ancestor .pi settings leaked into Load: %q", s.DefaultModel)
	}
}

func TestDefaultsFunc(t *testing.T) {
	d := Defaults()
	if !strings.Contains(d.SteeringMode, "one-at-a-time") || d.SteeringMode != "one-at-a-time" {
		t.Errorf("steeringMode default wrong: %q", d.SteeringMode)
	}
	if d.Compaction.Enabled != true || d.Retry.MaxRetries != 3 {
		t.Errorf("Defaults() wrong: %+v", d)
	}
}
