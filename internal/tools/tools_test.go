package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempCwd(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world\nline two\n"), 0o644)
	return dir
}

func TestBash(t *testing.T) {
	tool := Bash(t.TempDir())
	res := tool.Run(context.Background(), map[string]any{"command": "echo hello"})
	if res.IsError || strings.TrimSpace(res.Output) != "hello" {
		t.Fatalf("res = %+v", res)
	}
}

func TestBashFailureReportsCode(t *testing.T) {
	tool := Bash(t.TempDir())
	res := tool.Run(context.Background(), map[string]any{"command": "exit 3"})
	if !res.IsError || !strings.Contains(res.Output, "code 3") {
		t.Fatalf("res = %+v", res)
	}
}

func TestBashTimeout(t *testing.T) {
	tool := Bash(t.TempDir())
	res := tool.Run(context.Background(), map[string]any{"command": "sleep 5", "timeout": 1})
	if !res.IsError || !strings.Contains(res.Output, "timed out") {
		t.Fatalf("res = %+v", res)
	}
}

func TestBashUsesCwd(t *testing.T) {
	dir := tempCwd(t)
	tool := Bash(dir)
	res := tool.Run(context.Background(), map[string]any{"command": "ls a.txt"})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
}

func TestRead(t *testing.T) {
	dir := tempCwd(t)
	tool := Read(dir)
	res := tool.Run(context.Background(), map[string]any{"path": "a.txt"})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Output, "1\thello world") || !strings.Contains(res.Output, "2\tline two") {
		t.Fatalf("output = %q", res.Output)
	}
}

func TestReadWindow(t *testing.T) {
	dir := tempCwd(t)
	tool := Read(dir)
	res := tool.Run(context.Background(), map[string]any{"path": "a.txt", "offset": 2, "limit": 1})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	if strings.Contains(res.Output, "hello world") || !strings.Contains(res.Output, "2\tline two") {
		t.Fatalf("output = %q", res.Output)
	}
}

func TestReadMissing(t *testing.T) {
	tool := Read(t.TempDir())
	res := tool.Run(context.Background(), map[string]any{"path": "nope.txt"})
	if !res.IsError {
		t.Fatalf("expected error, got %+v", res)
	}
}

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	tool := Write(dir)
	res := tool.Run(context.Background(), map[string]any{"path": "sub/b.txt", "content": "content"})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sub", "b.txt"))
	if err != nil || string(data) != "content" {
		t.Fatalf("file = %q err = %v", data, err)
	}
}

func TestEdit(t *testing.T) {
	dir := tempCwd(t)
	tool := Edit(dir)
	res := tool.Run(context.Background(), map[string]any{
		"path":    "a.txt",
		"oldText": "hello world",
		"newText": "hello brave new world",
	})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if !strings.Contains(string(data), "hello brave new world") {
		t.Fatalf("content = %q", data)
	}
}

func TestEditNotFound(t *testing.T) {
	dir := tempCwd(t)
	tool := Edit(dir)
	res := tool.Run(context.Background(), map[string]any{
		"path":    "a.txt",
		"oldText": "missing text",
		"newText": "x",
	})
	if !res.IsError || !strings.Contains(res.Output, "not found") {
		t.Fatalf("res = %+v", res)
	}
}

func TestEditAmbiguous(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "dup.txt"), []byte("aaa\nbbb\naaa\n"), 0o644)
	tool := Edit(dir)
	res := tool.Run(context.Background(), map[string]any{
		"path":    "dup.txt",
		"oldText": "aaa",
		"newText": "xxx",
	})
	if !res.IsError || !strings.Contains(res.Output, "multiple locations") {
		t.Fatalf("res = %+v", res)
	}
}

func TestGrep(t *testing.T) {
	dir := tempCwd(t)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "b.go"), []byte("func main() {}\n"), 0o644)
	tool := Grep(dir)
	res := tool.Run(context.Background(), map[string]any{"pattern": "main"})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Output, "sub/b.go:1:") {
		t.Fatalf("output = %q", res.Output)
	}
	if strings.Contains(res.Output, "a.txt") {
		t.Fatalf("grep should not match a.txt: %q", res.Output)
	}
}

func TestGrepNoMatch(t *testing.T) {
	tool := Grep(t.TempDir())
	res := tool.Run(context.Background(), map[string]any{"pattern": "zzz"})
	if res.IsError || !strings.Contains(res.Output, "no matches") {
		t.Fatalf("res = %+v", res)
	}
}

func TestGlobDoublestar(t *testing.T) {
	dir := tempCwd(t)
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(dir, "a", "b", "c.go"), []byte("x"), 0o644)
	tool := Glob(dir)
	res := tool.Run(context.Background(), map[string]any{"pattern": "**/*.go"})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Output, "a/b/c.go") {
		t.Fatalf("output = %q", res.Output)
	}
	if strings.Contains(res.Output, "a.txt") {
		t.Fatalf("glob should not match a.txt: %q", res.Output)
	}
}

func TestGlobSingleStar(t *testing.T) {
	dir := tempCwd(t)
	os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "y.txt"), []byte("y"), 0o644)
	tool := Glob(dir)
	res := tool.Run(context.Background(), map[string]any{"pattern": "*.txt"})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Output, "x.txt") || strings.Contains(res.Output, "sub/y.txt") {
		t.Fatalf("output = %q", res.Output)
	}
}

func TestRegistry(t *testing.T) {
	r := New(Bash("/tmp"), Read("/tmp"))
	if _, ok := r.Get("bash"); !ok {
		t.Fatal("missing bash")
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("unexpected tool")
	}
	specs := r.Specs()
	if len(specs) != 2 {
		t.Fatalf("specs = %d", len(specs))
	}
}

func TestDefaultSet(t *testing.T) {
	r := New(Default(Deps{Cwd: "/tmp"})...)
	want := []string{"bash", "edit", "glob", "grep", "memory", "question", "read", "sessions", "web_search", "write"}
	got := r.List()
	if len(got) != len(want) {
		t.Fatalf("got %d tools: %v", len(got), toolNames(got))
	}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("order: got %v, want %v", toolNames(got), want)
		}
	}
}

func toolNames(ts []Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}
