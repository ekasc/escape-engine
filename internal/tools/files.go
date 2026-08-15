package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	readMaxLines  = 2000
	readMaxOutput = 50_000
)

// Read prints a file with line numbers, optionally windowed by offset/limit.
func Read(cwd string) Tool {
	return Tool{
		Name:        "read",
		Description: "Read a file with line numbers. Use offset and limit to window large files.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "File path (absolute or relative to the workspace)"},
				"offset": map[string]any{"type": "number", "description": "1-based line to start from (default 1)"},
				"limit":  map[string]any{"type": "number", "description": "Max lines to return (default 2000)"},
			},
			"required": []string{"path"},
		},
		Run: func(ctx context.Context, args map[string]any) Result {
			var p struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := decodeArgs(args, &p); err != nil {
				return errResult(err)
			}
			path, err := resolve(cwd, p.Path)
			if err != nil {
				return errResult(err)
			}
			offset := p.Offset
			if offset < 1 {
				offset = 1
			}
			limit := p.Limit
			if limit <= 0 {
				limit = readMaxLines
			}
			if limit > readMaxLines {
				limit = readMaxLines
			}
			return readWindow(path, offset, limit)
		},
	}
}

func readWindow(path string, offset, limit int) Result {
	data, err := os.ReadFile(path)
	if err != nil {
		return errResult(fmt.Errorf("read %s: %w", path, err))
	}
	lines := strings.Split(string(data), "\n")
	var out strings.Builder
	wrote := 0
	for i := offset - 1; i < len(lines) && wrote < limit; i++ {
		if out.Len() >= readMaxOutput {
			out.WriteString("…[truncated]\n")
			break
		}
		fmt.Fprintf(&out, "%d\t%s\n", i+1, lines[i])
		wrote++
	}
	if wrote == 0 && offset > len(lines) {
		return Result{Output: fmt.Sprintf("read %s: offset %d beyond end of file (%d lines)", path, offset, len(lines))}
	}
	return Result{Output: out.String()}
}

// Write creates or overwrites a file, creating parent directories.
func Write(cwd string) Tool {
	return Tool{
		Name:        "write",
		Description: "Create or overwrite a file with the given content. Creates parent directories as needed.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "File path (absolute or relative to the workspace)"},
				"content": map[string]any{"type": "string", "description": "Full file content"},
			},
			"required": []string{"path", "content"},
		},
		Run: func(ctx context.Context, args map[string]any) Result {
			var p struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := decodeArgs(args, &p); err != nil {
				return errResult(err)
			}
			path, err := resolve(cwd, p.Path)
			if err != nil {
				return errResult(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return errResult(fmt.Errorf("write %s: %w", path, err))
			}
			if err := os.WriteFile(path, []byte(p.Content), 0o644); err != nil {
				return errResult(fmt.Errorf("write %s: %w", path, err))
			}
			return Result{Output: fmt.Sprintf("Wrote %d bytes to %s", len(p.Content), path)}
		},
	}
}

// Grep searches files recursively for a regexp.
func Grep(cwd string) Tool {
	return Tool{
		Name:        "grep",
		Description: "Search files for a regular expression. Skips .git and binary files. Results are file:line:text.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Regular expression to search for"},
				"path":    map[string]any{"type": "string", "description": "Root to search (default: workspace)"},
			},
			"required": []string{"pattern"},
		},
		Run: func(ctx context.Context, args map[string]any) Result {
			var p struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
			}
			if err := decodeArgs(args, &p); err != nil {
				return errResult(err)
			}
			re, err := regexp.Compile(p.Pattern)
			if err != nil {
				return errResult(fmt.Errorf("grep: bad pattern: %w", err))
			}
			root := cwd
			if p.Path != "" {
				root, err = resolve(cwd, p.Path)
				if err != nil {
					return errResult(err)
				}
			}
			info, err := os.Stat(root)
			if err != nil {
				return errResult(fmt.Errorf("grep %s: %w", root, err))
			}
			if !info.IsDir() {
				return errResult(fmt.Errorf("grep: %s is not a directory", root))
			}
			return grepTree(ctx, root, re)
		},
	}
}

func grepTree(ctx context.Context, root string, re *regexp.Regexp) Result {
	var out strings.Builder
	matches := 0
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || ctx.Err() != nil {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if isBinary(path) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				rel, _ := filepath.Rel(root, path)
				fmt.Fprintf(&out, "%s:%d: %s\n", rel, i+1, strings.TrimRight(line, "\r"))
				matches++
				if matches >= 500 || out.Len() >= readMaxOutput {
					out.WriteString("…[truncated]\n")
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if out.Len() == 0 {
		return Result{Output: fmt.Sprintf("grep: no matches for %q", re.String())}
	}
	return Result{Output: out.String()}
}

func isBinary(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	buf := make([]byte, 8192)
	n, _ := f.Read(buf)
	return strings.IndexByte(string(buf[:n]), 0) >= 0
}

// Glob lists files matching a glob pattern (supports ** recursion).
func Glob(cwd string) Tool {
	return Tool{
		Name:        "glob",
		Description: "List files matching a glob pattern relative to the workspace. Use ** for recursive matches (e.g. **/*.go).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Glob pattern, e.g. **/*.go"},
			},
			"required": []string{"pattern"},
		},
		Run: func(ctx context.Context, args map[string]any) Result {
			var p struct {
				Pattern string `json:"pattern"`
			}
			if err := decodeArgs(args, &p); err != nil {
				return errResult(err)
			}
			pats := splitPattern(p.Pattern)
			var out strings.Builder
			count := 0
			filepath.Walk(cwd, func(path string, info os.FileInfo, err error) error {
				if err != nil || ctx.Err() != nil {
					return nil
				}
				if info.IsDir() {
					if info.Name() == ".git" {
						return filepath.SkipDir
					}
					return nil
				}
				rel, _ := filepath.Rel(cwd, path)
				if matchSegs(pats, splitPattern(rel)) {
					out.WriteString(rel + "\n")
					count++
					if count >= 1000 {
						out.WriteString("…[truncated]\n")
						return filepath.SkipAll
					}
				}
				return nil
			})
			if out.Len() == 0 {
				return Result{Output: fmt.Sprintf("glob: no files match %q", p.Pattern)}
			}
			return Result{Output: out.String()}
		},
	}
}

// resolve maps a tool path argument (absolute or relative to cwd) to an
// absolute path.
func resolve(cwd, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	return filepath.Abs(filepath.Join(cwd, p))
}

func splitPattern(p string) []string {
	segs := strings.Split(filepath.ToSlash(p), "/")
	out := segs[:0]
	for _, s := range segs {
		if s != "" && s != "." {
			out = append(out, s)
		}
	}
	return out
}

// matchSegs matches pattern segments against path segments with ** support.
func matchSegs(pats, segs []string) bool {
	for len(pats) > 0 {
		switch pats[0] {
		case "**":
			if matchSegs(pats[1:], segs) {
				return true
			}
			if len(segs) == 0 {
				return false
			}
			segs = segs[1:]
		default:
			if len(segs) == 0 {
				return false
			}
			ok, _ := filepath.Match(pats[0], segs[0])
			if !ok {
				return false
			}
			pats = pats[1:]
			segs = segs[1:]
		}
	}
	return len(segs) == 0
}
