package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportJSONL(t *testing.T) {
	path := writeTestSession(t)
	out := filepath.Join(t.TempDir(), "exported.jsonl")
	if err := ExportJSONL(path, out); err != nil {
		t.Fatal(err)
	}

	entries, err := ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	// Header + 5 entries (session_info, user, assistant, toolResult, assistant).
	if len(entries) != 6 {
		t.Fatalf("got %d entries, want 6", len(entries))
	}
	if entries[0].Type != TypeSession {
		t.Errorf("first entry type = %q, want session", entries[0].Type)
	}

	// Original session id must be preserved so the export keeps its identity.
	src, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].ID != src[0].ID {
		t.Errorf("exported id = %q, want %q", entries[0].ID, src[0].ID)
	}
	if entries[0].Cwd != "/tmp/proj" || entries[0].Version != 3 {
		t.Errorf("header = %+v", entries[0])
	}

	// parentIds re-chained linearly: first non-header entry has no parent,
	// then each entry points at the previous one.
	if entries[1].ParentID != "" {
		t.Errorf("first entry parentId = %q, want omitted", entries[1].ParentID)
	}
	prev := entries[1].ID
	for i := 2; i < len(entries); i++ {
		if entries[i].ParentID != prev {
			t.Errorf("entry %d parentId = %q, want %q", i, entries[i].ParentID, prev)
		}
		prev = entries[i].ID
	}

	// Content intact (2 assistant messages, usage preserved).
	var asst int
	var usage *Usage
	for _, e := range entries {
		if e.Type == TypeMessage && e.Message != nil && e.Message.Role == RoleAssistant {
			asst++
			usage = e.Message.Usage
		}
	}
	if asst != 2 || usage == nil || usage.Input != 5000 {
		t.Errorf("assistant messages = %d, last usage = %+v", asst, usage)
	}
}

func TestExportJSONLNoHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nohdr.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"x"}]}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ExportJSONL(path, filepath.Join(t.TempDir(), "out.jsonl")); err == nil {
		t.Error("expected error exporting a headerless file")
	}
}

func TestExportJSONLEmptyOut(t *testing.T) {
	if err := ExportJSONL(filepath.Join(t.TempDir(), "x.jsonl"), ""); err == nil {
		t.Error("expected error for empty out path")
	}
	if err := ExportHTML(filepath.Join(t.TempDir(), "x.jsonl"), ""); err == nil {
		t.Error("expected error for empty out path")
	}
}

func TestExportHTML(t *testing.T) {
	path := writeTestSession(t)
	out := filepath.Join(t.TempDir(), "session.html")
	if err := ExportHTML(path, out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)

	for _, want := range []string{
		"<!DOCTYPE html>",
		"<title>Test session</title>",
		"Test session",
		"session <code>",
		"/tmp/proj",
		"deepseek-v4-flash",
		"gpt-5.6-luna",
		"User",
		"Assistant",
		"Tool result",
		`<pre class="text">hi</pre>`,
		`class="toolcall"`,
		"read",
		"bash",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	if !strings.HasPrefix(html, "<!DOCTYPE html>") || !strings.HasSuffix(strings.TrimSpace(html), "</html>") {
		t.Error("HTML not well-formed")
	}
}

func TestExportHTMLEscapes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	st, err := Open(path, "/tmp/p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(Entry{Type: TypeMessage, Message: &Message{
		Role:    RoleUser,
		Content: []Block{{Type: BlockText, Text: `<script>alert("xss")</script>`}},
	}}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	out := filepath.Join(t.TempDir(), "s.html")
	if err := ExportHTML(path, out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "<script>") {
		t.Error("user text was not HTML-escaped")
	}
	if !strings.Contains(string(raw), "&lt;script&gt;") {
		t.Error("expected escaped script tag")
	}
}

func TestExportHTMLRendersThinkingCompactionAndImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	st, err := Open(path, "/tmp/p")
	if err != nil {
		t.Fatal(err)
	}
	appendE := func(e Entry) {
		t.Helper()
		if _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	appendE(Entry{Type: TypeMessage, Message: &Message{
		Role: RoleAssistant,
		Content: []Block{
			{Type: BlockThinking, Thinking: "let me think"},
			{Type: BlockText, Text: "answer"},
		},
	}})
	appendE(Entry{Type: TypeCompaction, Summary: "compacted summary", TokensBefore: 12345, FirstKeptEntryID: "x1"})
	appendE(Entry{Type: TypeBranchSummary, Summary: "branch summary", FromID: "x0"})
	appendE(Entry{Type: TypeMessage, Message: &Message{
		Role:    RoleUser,
		Content: []Block{{Type: BlockImage, MimeType: "image/png", Data: "iVBORw0KGgo="}},
	}})
	appendE(Entry{Type: TypeModelChange, ModelID: "gpt-5.6-terra"})
	appendE(Entry{Type: TypeSessionInfo, Name: "Renamed"})
	st.Close()

	out := filepath.Join(t.TempDir(), "s.html")
	if err := ExportHTML(path, out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	h := string(raw)
	for _, want := range []string{
		"let me think",
		"compacted summary",
		"12345 tokens before",
		"branch summary",
		`src="data:image/png;base64,iVBORw0KGgo="`,
		"Model changed to gpt-5.6-terra",
		"Session renamed to Renamed",
		"<title>Renamed</title>",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
}

// TestExportJSONLRoundTrip parses the exported file with the strict JSON
// decoder to guarantee every line is valid JSON (what the desktop shell/pi readers do).
func TestExportJSONLRoundTrip(t *testing.T) {
	path := writeTestSession(t)
	out := filepath.Join(t.TempDir(), "roundtrip.jsonl")
	if err := ExportJSONL(path, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 6 {
		t.Fatalf("got %d lines, want 6", len(lines))
	}
	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d not valid JSON: %v", i, err)
		}
	}
}
