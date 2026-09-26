package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/ekasc/escape/engine/internal/session"
)

// Sessions lets the agent find and resume earlier work in this project instead
// of making the user hunt through a session list.
//
// Resuming is deferred, not immediate: the engine runs one agent at a time, so
// switching sessions stops the agent that is running this very tool. The request
// is recorded and applied once the turn settles, so the tool reports back what
// is about to happen and the next message lands in the resumed session.
func Sessions(root, cwd string, currentPath func() string, ctrl *session.Control) Tool {
	return Tool{
		Name:        "sessions",
		Description: "Find earlier sessions in this project and resume one. op=search matches a phrase against session names, first messages, and transcript text; op=list shows the most recent; op=resume switches to a session by id or path, which takes effect after this turn ends.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"op": map[string]any{
					"type":        "string",
					"enum":        []string{"search", "list", "resume"},
					"description": "search finds sessions matching a phrase, list shows recent ones, resume switches to one.",
				},
				"query":   map[string]any{"type": "string", "description": "Phrase to search for. Omit or leave empty with op=list."},
				"session": map[string]any{"type": "string", "description": "Session id or path from a search result. Required for op=resume."},
				"limit":   map[string]any{"type": "integer", "description": "Maximum results. Defaults to 10."},
			},
			"required": []string{"op"},
		},
		// Not parallel-safe: resume mutates engine state, and search reads
		// session files that a resume may be rewriting.
		ParallelSafe: false,
		Run: func(_ context.Context, args map[string]any) Result {
			var p struct {
				Op      string `json:"op"`
				Query   string `json:"query"`
				Session string `json:"session"`
				Limit   int    `json:"limit"`
			}
			if err := decodeArgs(args, &p); err != nil {
				return Result{Output: "invalid sessions arguments: " + err.Error(), IsError: true}
			}

			current := currentPath()
			switch p.Op {
			case "list":
				p.Query = ""
				fallthrough
			case "search":
				matches, err := session.Search(root, cwd, p.Query, p.Limit)
				if err != nil {
					return Result{Output: "session search failed: " + err.Error(), IsError: true}
				}
				return Result{Output: formatMatches(matches, current)}
			case "resume":
				return resumeSession(root, cwd, current, p.Session, ctrl)
			default:
				return Result{Output: "op must be one of: search, list, resume", IsError: true}
			}
		},
	}
}

func resumeSession(root, cwd, currentPath, ref string, ctrl *session.Control) Result {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Result{Output: "op=resume needs a session id or path from a previous result", IsError: true}
	}
	infos, err := session.ListForCwd(root, cwd)
	if err != nil {
		return Result{Output: "session search failed: " + err.Error(), IsError: true}
	}
	var target *session.Info
	for i := range infos {
		info := infos[i]
		if info.ID == ref || info.Path == ref || filepath.Base(info.Path) == ref {
			target = &info
			break
		}
	}
	if target == nil {
		return Result{Output: fmt.Sprintf("no session in this project matches %q; run op=search to see candidates", ref), IsError: true}
	}
	if currentPath != "" && filepath.Clean(target.Path) == filepath.Clean(currentPath) {
		return Result{Output: "already in that session"}
	}
	ctrl.RequestResume(target.Path)
	return Result{Output: fmt.Sprintf("resuming %q after this turn ends. The next message lands there; do not assume the current session continues.", target.Name)}
}

func formatMatches(matches []session.SearchMatch, currentPath string) string {
	if len(matches) == 0 {
		return "no sessions in this project matched"
	}
	var b strings.Builder
	for i, m := range matches {
		name := m.Name
		if name == "" {
			name = "(untitled)"
		}
		here := ""
		if currentPath != "" && filepath.Clean(m.Path) == filepath.Clean(currentPath) {
			here = "  <- current session"
		}
		fmt.Fprintf(&b, "%d. %s\n   id: %s\n   last touched: %s  (matched: %s)\n",
			i+1, name, m.ID, relativeTime(m.Mtime), m.MatchedIn)
		if m.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", m.Snippet)
		}
		b.WriteString(here + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func relativeTime(unixMillis int64) string {
	if unixMillis <= 0 {
		return "unknown"
	}
	d := time.Since(time.UnixMilli(unixMillis))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return time.UnixMilli(unixMillis).Format("2006-01-02")
	}
}
