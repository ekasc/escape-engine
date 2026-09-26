package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekasc/escape-engine/internal/memory"
	"github.com/ekasc/escape-engine/internal/session"
)

func TestLatestSessionForCwd(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	oldPath := filepath.Join(root, "project", "old.jsonl")
	newPath := filepath.Join(root, "project", "new.jsonl")
	for _, path := range []string{oldPath, newPath} {
		store, err := session.Open(path, cwd)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-time.Hour)
	newTime := time.Now()
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newPath, newTime, newTime); err != nil {
		t.Fatal(err)
	}

	info, err := latestSessionForCwd(root, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if info == nil || info.Path != newPath {
		t.Fatalf("latest session = %+v, want %s", info, newPath)
	}

	other, err := latestSessionForCwd(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if other != nil {
		t.Fatalf("session for another cwd = %+v, want nil", other)
	}
}

func TestConfiguredSessionRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := configuredSessionRoot("~/.escape/sessions"); got != filepath.Join(home, ".escape", "sessions") {
		t.Fatalf("tilde session root = %q", got)
	}
	if got := configuredSessionRoot("relative/sessions"); !filepath.IsAbs(got) {
		t.Fatalf("relative session root = %q, want absolute", got)
	}
	t.Setenv("ESCAPE_SESSIONS_DIR", filepath.Join(home, "fallback"))
	if got := configuredSessionRoot(""); got != filepath.Join(home, "fallback") {
		t.Fatalf("empty session root = %q", got)
	}
}

func TestToolsForCwd(t *testing.T) {
	all, err := toolsForCwd(t.TempDir(), nil, "", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("empty tool list")
	}
	selected, err := toolsForCwd(t.TempDir(), nil, "", nil, nil, []string{"bash", "read"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || selected[0].Name != "bash" || selected[1].Name != "read" {
		t.Fatalf("selected tools = %+v", selected)
	}
	if _, err := toolsForCwd(t.TempDir(), nil, "", nil, nil, []string{"teleport"}); err == nil {
		t.Fatal("unknown tool was accepted")
	}
}

// The memory tool must survive the name filter: a session that opts into it
// needs a working store, and Default always builds one.
func TestToolsForCwdSelectsMemory(t *testing.T) {
	store := memory.Open(t.TempDir(), t.TempDir())
	selected, err := toolsForCwd(t.TempDir(), store, "", nil, nil, []string{"memory"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].Name != "memory" {
		t.Fatalf("selected tools = %+v", selected)
	}
	res := selected[0].Run(context.Background(), map[string]any{"op": "add", "text": "kept"})
	if res.IsError {
		t.Fatalf("memory add: %s", res.Output)
	}
}

func TestWorkspaceDiff(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	git("init")
	git("config", "user.email", "escape@example.invalid")
	git("config", "user.name", "escape test")
	file := filepath.Join(repo, "tracked.txt")
	if err := os.WriteFile(file, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.txt")
	git("commit", "-m", "initial")
	if err := os.WriteFile(file, []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stagedFile := filepath.Join(repo, "staged.txt")
	if err := os.WriteFile(stagedFile, []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "staged.txt")
	untrackedFile := filepath.Join(repo, "untracked.txt")
	if err := os.WriteFile(untrackedFile, []byte("new file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := workspaceDiff(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "+after") || !strings.Contains(diff, "-before") {
		t.Fatalf("diff = %q", diff)
	}
	if !strings.Contains(diff, "--- staged ---") || !strings.Contains(diff, "+staged") {
		t.Fatalf("staged diff missing: %q", diff)
	}
	if !strings.Contains(diff, "--- untracked untracked.txt ---") || !strings.Contains(diff, "+new file") {
		t.Fatalf("untracked diff missing: %q", diff)
	}
	scoped, err := workspaceDiff(repo, "tracked.txt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(scoped, "staged.txt") || strings.Contains(scoped, "untracked.txt") || !strings.Contains(scoped, "+after") {
		t.Fatalf("scoped diff = %q", scoped)
	}
}

func TestReplResumeFlag(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	path := filepath.Join(root, "project", "resume.jsonl")
	store, err := session.Open(path, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ESCAPE_SESSIONS_DIR", root)

	var stdout, stderr strings.Builder
	if code := run([]string{"repl", "--fake", "--cwd", cwd, "--resume"}, strings.NewReader("/exit\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("run --resume exit = %d, stderr = %s", code, stderr.String())
	}
}

func TestAskUsesSettingsModelDefault(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ESCAPE_SESSIONS_DIR", root)
	settingsDir := filepath.Join(cwd, ".escape")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"defaultModel":"ask-model"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	if code := run([]string{"ask", "--fake", "--cwd", cwd, "hello"}, &strings.Reader{}, &stdout, &stderr); code != 0 {
		t.Fatalf("ask exit = %d, stderr = %s", code, stderr.String())
	}
	infos, err := session.List(root)
	if err != nil || len(infos) != 1 {
		t.Fatalf("sessions = %+v, err=%v", infos, err)
	}
	entries, err := session.ReadAll(infos[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if entry.Message != nil && entry.Message.Role == session.RoleAssistant && entry.Message.Model == "ask-model" {
			found = true
		}
	}
	if !found {
		t.Fatalf("configured ask model not persisted: %+v", entries)
	}
}

func TestRPCCreatesSessionWhenPathOmitted(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ESCAPE_SESSIONS_DIR", root)
	var stdout, stderr strings.Builder
	request := `{"type":"get_state"}` + "\n"
	if code := run([]string{"rpc", "--fake", "--cwd", cwd}, strings.NewReader(request), &stdout, &stderr); code != 0 {
		t.Fatalf("rpc exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"command":"get_state"`) || !strings.Contains(stdout.String(), `"success":true`) {
		t.Fatalf("rpc stdout = %q", stdout.String())
	}
	infos, err := session.List(root)
	if err != nil || len(infos) != 1 {
		t.Fatalf("created sessions = %+v, err=%v", infos, err)
	}
}

func TestAskRejectsApprovalSettings(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("HOME", home)
	settingsDir := filepath.Join(cwd, ".escape")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"approvalMode":"ask"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	if code := run([]string{"ask", "--fake", "--cwd", cwd, "hello"}, &strings.Reader{}, &strings.Builder{}, &stderr); code != 2 {
		t.Fatalf("ask exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "interactive repl") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if code := run([]string{"ask", "--fake", "--cwd", cwd, "--approval", "auto", "hello"}, &strings.Reader{}, &strings.Builder{}, &strings.Builder{}); code != 0 {
		t.Fatalf("explicit auto ask exit = %d", code)
	}
}

func TestReplUsesSettingsProviderDefault(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ESCAPE_PROVIDER", "")
	t.Setenv("ESCAPE_API_KEY", "test-key")
	settingsDir := filepath.Join(cwd, ".escape")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"defaultProvider":"api"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	if code := run([]string{"repl", "--cwd", cwd}, strings.NewReader("/state\n/exit\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("run exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "state: idle") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestReplUsesSettingsModelDefault(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("HOME", home)
	settingsDir := filepath.Join(cwd, ".escape")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"defaultModel":"configured-model","defaultThinkingLevel":"high"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	if code := run([]string{"repl", "--fake", "--cwd", cwd}, strings.NewReader("/state\n/exit\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("run exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "model=configured-model") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	stdout.Reset()
	if code := run([]string{"repl", "--fake", "--cwd", cwd, "--model", "cli-model"}, strings.NewReader("/state\n/exit\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("CLI model run exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "model=cli-model") {
		t.Fatalf("CLI model stdout = %q", stdout.String())
	}
}

func TestReplApprovalSettingAndCLIPrecedence(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("HOME", home)
	settingsDir := filepath.Join(cwd, ".escape")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"approvalMode":"ask"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr strings.Builder
	if code := run([]string{"repl", "--fake", "--cwd", cwd}, strings.NewReader("/exit\n"), &strings.Builder{}, &stderr); code != 2 {
		t.Fatalf("settings approval exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "interactive terminal") {
		t.Fatalf("settings approval stderr = %q", stderr.String())
	}

	stderr.Reset()
	if code := run([]string{"repl", "--fake", "--cwd", cwd, "--approval", "auto"}, strings.NewReader("/exit\n"), &strings.Builder{}, &stderr); code != 0 {
		t.Fatalf("CLI override exit = %d, want 0, stderr = %s", code, stderr.String())
	}
}

func TestReplApprovalAskRequiresInteractiveInput(t *testing.T) {
	var stderr strings.Builder
	if code := run([]string{"repl", "--fake", "--approval", "ask"}, strings.NewReader("/exit\n"), &strings.Builder{}, &stderr); code != 2 {
		t.Fatalf("run exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "interactive terminal") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestReplRejectsSessionAndResumeTogether(t *testing.T) {
	var stderr strings.Builder
	if code := run([]string{"repl", "--fake", "--session", "session.jsonl", "--resume"}, strings.NewReader(""), &strings.Builder{}, &stderr); code != 2 {
		t.Fatalf("run exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "only one") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// The shell launches `escape rpc` with no session. Resolving to a fresh path
// every launch is what made a project accumulate one session per open, so an
// unattached rpc must land on the project's most recent session instead.
//
// ESCAPE_SESSIONS_DIR points DefaultRoot at a scratch directory, so this can
// never write into the real store.
func TestResolveLiveSessionPathPrefersLatest(t *testing.T) {
	t.Setenv("ESCAPE_SESSIONS_DIR", t.TempDir())
	root := session.DefaultRoot()
	cwd := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	older := filepath.Join(root, session.SlugForDir(cwd), "2026-01-01T00-00-00-000Z_old.jsonl")
	newer := filepath.Join(root, session.SlugForDir(cwd), "2026-06-01T00-00-00-000Z_new.jsonl")
	for _, p := range []string{older, newer} {
		store, err := session.Open(p, cwd)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// List sorts on mtime, so make the intended winner the newer file.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(newer, future, future); err != nil {
		t.Fatal(err)
	}

	got, err := resolveLiveSessionPath(cwd, root)
	if err != nil {
		t.Fatal(err)
	}
	if got != newer {
		t.Fatalf("expected the most recent session %q, got %q", newer, got)
	}
}

func TestResolveLiveSessionPathCreatesWhenNoHistory(t *testing.T) {
	t.Setenv("ESCAPE_SESSIONS_DIR", t.TempDir())
	root := session.DefaultRoot()
	cwd := filepath.Join(t.TempDir(), "fresh")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveLiveSessionPath(cwd, root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, ".jsonl") {
		t.Fatalf("expected a new session path, got %q", got)
	}
	// NewPath only builds a path; the directory appears when the session opens.
	if !strings.HasPrefix(got, root) {
		t.Fatalf("a new session must sit under the configured root %q, got %q", root, got)
	}
}
