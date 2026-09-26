package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ekasc/escape-engine/internal/settings"
)

func TestNormalizeLoginProvider(t *testing.T) {
	cases := map[string]string{
		"opencode-go":  string(loginOpenCodeGo),
		"go":           string(loginOpenCodeGo),
		"opencode-zen": string(loginOpenCodeZen),
		"zen":          string(loginOpenCodeZen),
		"opencode":     string(loginOpenCodeZen),
		"codex":        string(loginCodex),
		"chatgpt":      string(loginCodex),
	}
	for input, want := range cases {
		if got := normalizeLoginProvider(input); got != want {
			t.Errorf("normalizeLoginProvider(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestChooseLoginProviderAccessible(t *testing.T) {
	var output bytes.Buffer
	got, err := chooseLoginProvider(strings.NewReader("2\n"), &output, true)
	if err != nil {
		t.Fatalf("chooseLoginProvider: %v\n%s", err, output.String())
	}
	if got != string(loginOpenCodeZen) {
		t.Fatalf("provider = %q, want %q", got, loginOpenCodeZen)
	}
}

func TestProviderLoginCommandConnectsAndPersists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OPENCODE_GO_API_KEY", "test-key")
	cmd := &providerLoginCommand{target: string(loginOpenCodeGo), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	if err := cmd.Run(); err != nil {
		t.Fatalf("provider login: %v", err)
	}
	loaded, err := settings.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DefaultProvider != string(loginOpenCodeGo) {
		t.Fatalf("default provider = %q", loaded.DefaultProvider)
	}
}

func TestCmdLoginProviderFlagPersistsSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OPENCODE_ZEN_API_KEY", "test-key")
	var stdout, stderr bytes.Buffer
	if code := cmdLogin([]string{"--provider", "opencode-zen"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("login exit = %d, stderr = %s", code, stderr.String())
	}
	loaded, err := settings.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DefaultProvider != string(loginOpenCodeZen) {
		t.Fatalf("default provider = %q", loaded.DefaultProvider)
	}
}
