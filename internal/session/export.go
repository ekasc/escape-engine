package session

import (
	"encoding/json"
	"errors"
	"html"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ExportJSONL writes the session to out as a linear JSONL file: a fresh
// `session` header (id/cwd/timestamp/version taken from the source header so
// the exported session keeps its identity) followed by every entry with
// parentIds re-chained into a linear sequence. This mirrors pi's
// exportToJsonl, which exports the active branch only.
func ExportJSONL(path, out string) error {
	if out == "" {
		return errors.New("export path is empty")
	}
	entries, err := ReadAll(path)
	if err != nil {
		return err
	}
	var header *Entry
	for i := range entries {
		if entries[i].Type == TypeSession && entries[i].ID != "" {
			header = &entries[i]
			break
		}
	}
	if header == nil {
		return ErrEmpty
	}

	lines := make([][]byte, 0, len(entries)+1)
	h := Entry{
		Type:      TypeSession,
		Version:   3,
		ID:        header.ID,
		Timestamp: header.Timestamp,
		Cwd:       header.Cwd,
	}
	if header.Version != 0 {
		h.Version = header.Version
	}
	line, err := json.Marshal(h)
	if err != nil {
		return err
	}
	lines = append(lines, line)

	prevID := ""
	for _, e := range entries {
		if e.Type == TypeSession {
			continue // header written above; any nested session entries are dropped
		}
		e.ParentID = prevID
		prevID = e.ID
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		lines = append(lines, line)
	}
	return writeLines(out, lines)
}

// ExportHTML renders the session as a single self-contained HTML page: every
// message (user, assistant with thinking/tool calls, tool results, bash) plus
// session_info, model_change, compaction and branch_summary entries. All text
// is HTML-escaped.
func ExportHTML(path, out string) error {
	if out == "" {
		return errors.New("export path is empty")
	}
	entries, err := ReadAll(path)
	if err != nil {
		return err
	}

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	b.WriteString("<title>")
	b.WriteString(html.EscapeString(exportTitle(entries)))
	b.WriteString("</title>\n<style>\n")
	b.WriteString(exportCSS)
	b.WriteString("\n</style>\n</head>\n<body>\n")

	// Header: session id, cwd, name.
	var header *Entry
	for i := range entries {
		if entries[i].Type == TypeSession && entries[i].ID != "" {
			header = &entries[i]
			break
		}
	}
	b.WriteString("<header class=\"session-header\">\n")
	if header != nil {
		b.WriteString("<h1>")
		b.WriteString(html.EscapeString(exportTitle(entries)))
		b.WriteString("</h1>\n")
		b.WriteString("<div class=\"meta\">session <code>")
		b.WriteString(html.EscapeString(header.ID))
		b.WriteString("</code>")
		if header.Cwd != "" {
			b.WriteString(" &middot; cwd <code>")
			b.WriteString(html.EscapeString(header.Cwd))
			b.WriteString("</code>")
		}
		b.WriteString("</div>\n")
	} else {
		b.WriteString("<h1>Session</h1>\n")
	}
	b.WriteString("</header>\n<main>\n")

	lastModel := ""
	for _, e := range entries {
		if e.Message != nil && e.Message.Model != "" {
			lastModel = e.Message.Model
		} else if e.ModelID != "" {
			lastModel = e.ModelID
		}
		switch e.Type {
		case TypeSession:
			// header only
		case TypeSessionInfo:
			if strings.TrimSpace(e.Name) != "" {
				exportBanner(&b, "session_info", "Session renamed to "+html.EscapeString(strings.TrimSpace(e.Name)), e.Timestamp)
			}
		case TypeModelChange:
			model := e.ModelID
			if model == "" {
				model = e.Model
			}
			if model != "" {
				exportBanner(&b, "model_change", "Model changed to "+html.EscapeString(model), e.Timestamp)
			}
		case TypeCompaction:
			exportSummary(&b, "compaction", "Compaction", e.Summary, e.TokensBefore, e.FirstKeptEntryID, e.Timestamp)
		case TypeBranchSummary:
			exportSummary(&b, "branch_summary", "Branch summary", e.Summary, 0, e.FromID, e.Timestamp)
		case TypeMessage:
			if e.Message != nil {
				exportMessage(&b, e, lastModel)
			}
		default:
			// Unknown entry types are skipped; the JSONL export keeps them.
		}
	}

	b.WriteString("</main>\n</body>\n</html>\n")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(out, []byte(b.String()), 0o644)
}

func exportTitle(entries []Entry) string {
	if name := LastName(entries); name != "" {
		return name
	}
	for _, e := range entries {
		if e.Type == TypeSession && e.ID != "" {
			return "Session " + e.ID
		}
	}
	return "Session"
}

func exportBanner(b *strings.Builder, class, text, timestamp string) {
	b.WriteString("<div class=\"entry banner ")
	b.WriteString(class)
	b.WriteString("\"><span class=\"label\">")
	b.WriteString(class)
	b.WriteString("</span><span class=\"text\">")
	b.WriteString(text)
	b.WriteString("</span>")
	if timestamp != "" {
		b.WriteString("<span class=\"time\">")
		b.WriteString(html.EscapeString(timestamp))
		b.WriteString("</span>")
	}
	b.WriteString("</div>\n")
}

func exportSummary(b *strings.Builder, class, label, summary string, tokensBefore int, refID, timestamp string) {
	b.WriteString("<div class=\"entry ")
	b.WriteString(class)
	b.WriteString("\"><div class=\"head\"><span class=\"label\">")
	b.WriteString(label)
	b.WriteString("</span>")
	if tokensBefore > 0 {
		b.WriteString("<span class=\"time\">")
		b.WriteString(itoa(tokensBefore))
		b.WriteString(" tokens before</span>")
	}
	if refID != "" {
		b.WriteString("<span class=\"time\">from <code>")
		b.WriteString(html.EscapeString(refID))
		b.WriteString("</code></span>")
	}
	if timestamp != "" {
		b.WriteString("<span class=\"time\">")
		b.WriteString(html.EscapeString(timestamp))
		b.WriteString("</span>")
	}
	b.WriteString("</div><pre class=\"summary\">")
	b.WriteString(html.EscapeString(summary))
	b.WriteString("</pre></div>\n")
}

func exportMessage(b *strings.Builder, e Entry, lastModel string) {
	msg := e.Message
	class := "message"
	switch msg.Role {
	case RoleUser:
		class += " user"
	case RoleAssistant:
		class += " assistant"
	case RoleToolResult:
		class += " toolResult"
	case RoleBashExecution:
		class += " bash"
	}
	b.WriteString("<div class=\"entry ")
	b.WriteString(class)
	b.WriteString("\"><div class=\"head\"><span class=\"label\">")
	b.WriteString(roleLabel(msg.Role))
	b.WriteString("</span>")
	model := msg.Model
	if model == "" {
		model = lastModel
	}
	if model != "" {
		b.WriteString("<span class=\"model\">")
		b.WriteString(html.EscapeString(model))
		b.WriteString("</span>")
	}
	if msg.ToolName != "" {
		b.WriteString("<span class=\"model\">")
		b.WriteString(html.EscapeString(msg.ToolName))
		b.WriteString("</span>")
	}
	if msg.IsError {
		b.WriteString("<span class=\"time\">error</span>")
	}
	if msg.Timestamp > 0 {
		b.WriteString("<span class=\"time\">")
		b.WriteString(html.EscapeString(time.UnixMilli(msg.Timestamp).UTC().Format("2006-01-02 15:04:05")))
		b.WriteString("</span>")
	}
	b.WriteString("</div>")

	for _, blk := range msg.Content {
		switch blk.Type {
		case BlockText:
			if blk.Text != "" {
				b.WriteString("<pre class=\"text\">")
				b.WriteString(html.EscapeString(blk.Text))
				b.WriteString("</pre>")
			}
		case BlockThinking:
			if blk.Thinking != "" {
				b.WriteString("<details class=\"thinking\"><summary>Thinking</summary><pre>")
				b.WriteString(html.EscapeString(blk.Thinking))
				b.WriteString("</pre></details>")
			}
		case BlockToolCall:
			args, err := json.Marshal(blk.Arguments)
			if err != nil || string(args) == "null" {
				args = []byte("{}")
			}
			b.WriteString("<div class=\"toolcall\"><code>")
			b.WriteString(html.EscapeString(blk.Name))
			b.WriteString("</code><pre>")
			b.WriteString(html.EscapeString(string(args)))
			b.WriteString("</pre></div>")
		case BlockImage:
			if blk.Data != "" {
				mime := blk.MimeType
				if mime == "" {
					mime = "image/png"
				}
				b.WriteString("<img alt=\"image\" src=\"data:")
				b.WriteString(html.EscapeString(mime))
				b.WriteString(";base64,")
				b.WriteString(html.EscapeString(blk.Data))
				b.WriteString("\">")
			} else if blk.Source != "" {
				b.WriteString("<img alt=\"image\" src=\"")
				b.WriteString(html.EscapeString(blk.Source))
				b.WriteString("\">")
			}
		}
	}
	b.WriteString("</div>\n")
}

func roleLabel(role string) string {
	switch role {
	case RoleUser:
		return "User"
	case RoleAssistant:
		return "Assistant"
	case RoleToolResult:
		return "Tool result"
	case RoleBashExecution:
		return "Bash"
	default:
		return role
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

func writeLines(path string, lines [][]byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, line := range lines {
		if _, err := f.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return nil
}

const exportCSS = `
:root { color-scheme: light dark; }
* { box-sizing: border-box; }
body { font-family: -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
       margin: 0; background: #fafafa; color: #1a1a1a; }
.session-header { padding: 24px 32px 16px; border-bottom: 1px solid #e5e5e5; background: #fff; }
.session-header h1 { margin: 0 0 6px; font-size: 20px; }
.meta { color: #666; font-size: 13px; }
main { max-width: 860px; margin: 0 auto; padding: 16px 24px 64px; }
.entry { background: #fff; border: 1px solid #e5e5e5; border-radius: 8px;
         margin: 12px 0; padding: 12px 16px; }
.entry .head { display: flex; gap: 12px; align-items: baseline; margin-bottom: 6px; flex-wrap: wrap; }
.label { font-weight: 600; font-size: 13px; text-transform: uppercase; letter-spacing: 0.04em; }
.entry.user { border-left: 3px solid #2f6fed; }
.entry.assistant { border-left: 3px solid #0a9d58; }
.entry.toolResult { border-left: 3px solid #b06000; background: #fdfaf5; }
.entry.bash { border-left: 3px solid #6f42c1; background: #faf7fd; }
.entry.banner { border-left: 3px solid #999; background: #f5f5f5; display: flex; gap: 12px; align-items: baseline; }
.entry.compaction, .entry.branch_summary { border-left: 3px solid #c62828; background: #fdf7f7; }
.model, .time { color: #888; font-size: 12px; }
code { background: #f0f0f0; border-radius: 4px; padding: 1px 5px; font-size: 12px; }
pre { white-space: pre-wrap; word-break: break-word; font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
      font-size: 13px; line-height: 1.5; margin: 6px 0 0; }
pre.text { white-space: pre-wrap; }
details.thinking summary { cursor: pointer; color: #666; font-size: 13px; margin-top: 6px; }
details.thinking pre { color: #555; font-style: italic; }
.toolcall { margin-top: 8px; border: 1px solid #e5e5e5; border-radius: 6px; padding: 8px 12px; background: #f8f9fa; }
.toolcall code { font-weight: 600; }
pre.summary { background: #fff3f3; border-radius: 6px; padding: 8px 12px; }
img { max-width: 100%; border-radius: 6px; margin-top: 8px; }
@media (prefers-color-scheme: dark) {
  body { background: #161616; color: #e8e8e8; }
  .session-header, .entry { background: #1e1e1e; border-color: #333; }
  .entry.toolResult { background: #241d12; }
  .entry.bash { background: #1d1826; }
  .entry.banner { background: #262626; }
  .entry.compaction, .entry.branch_summary { background: #261a1a; }
  code { background: #2c2c2c; }
  .toolcall { background: #262626; border-color: #333; }
  pre.summary { background: #2c1f1f; }
  .model, .time, details.thinking summary { color: #9a9a9a; }
}
`
