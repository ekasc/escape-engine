package provider

import (
	"os"
	"path/filepath"
	"testing"
)

func writeOpenCodeAuth(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenCodeGoKeyFromAuthStore(t *testing.T) {
	t.Setenv("OPENCODE_GO_API_KEY", "")
	writeOpenCodeAuth(t, `{"opencode-go":{"key":"sk-opencode-123"},"other":{"key":"sk-other"}}`)
	if got := OpenCodeGoKey(); got != "sk-opencode-123" {
		t.Fatalf("key = %q", got)
	}
}

func TestOpenCodeGoKeyEnvWins(t *testing.T) {
	writeOpenCodeAuth(t, `{"opencode-go":{"key":"sk-file"}}`)
	t.Setenv("OPENCODE_GO_API_KEY", "sk-env")
	if got := OpenCodeGoKey(); got != "sk-env" {
		t.Fatalf("key = %q, want env override", got)
	}
}

func TestOpenCodeGoKeyMissing(t *testing.T) {
	t.Setenv("OPENCODE_GO_API_KEY", "")
	t.Setenv("HOME", t.TempDir())
	if got := OpenCodeGoKey(); got != "" {
		t.Fatalf("key = %q, want empty", got)
	}
}

func TestOpenCodeZenKeyFromAuthStore(t *testing.T) {
	t.Setenv("OPENCODE_ZEN_API_KEY", "")
	writeOpenCodeAuth(t, `{"opencode":{"key":"sk-zen-123"}}`)
	if got := OpenCodeZenKey(); got != "sk-zen-123" {
		t.Fatalf("key = %q", got)
	}
}

func TestOpenCodeZenKeyEnvWins(t *testing.T) {
	writeOpenCodeAuth(t, `{"opencode":{"key":"sk-file"}}`)
	t.Setenv("OPENCODE_ZEN_API_KEY", "sk-env")
	if got := OpenCodeZenKey(); got != "sk-env" {
		t.Fatalf("key = %q, want env override", got)
	}
}

func TestEscapeCredentialStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ESCAPE_OPENCODE_GO_API_KEY", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")
	if err := SaveOpenCodeKey("opencode-go", "sk-escape"); err != nil {
		t.Fatal(err)
	}
	if got := OpenCodeGoKey(); got != "sk-escape" {
		t.Fatalf("key = %q, want Escape credential", got)
	}
	path := filepath.Join(home, ".escape", "credentials.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential permissions = %o, want 600", info.Mode().Perm())
	}
}
