package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoginOpenCodeUsesProviderID(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir()
	argsPath := filepath.Join(binDir, "args.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsPath + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("OPENCODE_ZEN_API_KEY", "")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := LoginOpenCode(context.Background(), OpenCodeLoginZen, strings.NewReader(""), os.Stdout, os.Stderr); err != nil {
		t.Fatalf("LoginOpenCode: %v", err)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(data)), "auth\nlogin\n--provider\nopencode"; got != want {
		t.Fatalf("opencode args = %q, want %q", got, want)
	}
}
