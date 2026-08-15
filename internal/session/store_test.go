package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.jsonl"), "/tmp/proj")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenWritesHeader(t *testing.T) {
	s := newTestStore(t)
	if s.ID() == "" {
		t.Fatal("expected session id from header")
	}
	entries, err := ReadAll(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Type != TypeSession {
		t.Fatalf("expected header entry, got %+v", entries)
	}
	if entries[0].Version != 3 || entries[0].Cwd != "/tmp/proj" {
		t.Fatalf("bad header: %+v", entries[0])
	}
}

// TestShapeCompatibility locks the JSONL shape to what Babylon's reader
// (electron/sessions.ts) expects: message entries with entry.message carrying
// role/content/timestamp, toolCallId on tool results, session_info with name.
func TestShapeCompatibility(t *testing.T) {
	s := newTestStore(t)

	user, err := s.Append(Entry{
		Type: TypeMessage,
		Message: &Message{
			Role:      RoleUser,
			Content:   []Block{{Type: BlockText, Text: "hello"}},
			Timestamp: 1784702694866,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if user.ID == "" || user.ParentID == "" || user.Timestamp == "" {
		t.Fatalf("expected filled id/parentId/timestamp: %+v", user)
	}

	assistant, err := s.Append(Entry{
		Type: TypeMessage,
		Message: &Message{
			Role:       RoleAssistant,
			Content:    []Block{{Type: BlockText, Text: "hi"}, {Type: BlockToolCall, ID: "call_1", Name: "bash", Arguments: map[string]any{"command": "ls"}}},
			Model:      "test-model",
			StopReason: StopToolUse,
			Timestamp:  1784702694867,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := s.Append(Entry{
		Type: TypeMessage,
		Message: &Message{
			Role:       RoleToolResult,
			ToolCallID: "call_1",
			ToolName:   "bash",
			Content:    []Block{{Type: BlockText, Text: "file.txt"}},
			IsError:    false,
			Timestamp:  1784702694868,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	info, err := s.Append(Entry{Type: TypeSessionInfo, Name: "Auto title", ParentID: assistant.ID})
	if err != nil {
		t.Fatal(err)
	}

	// Re-read raw lines and check with the same eyes as Babylon.
	raw, _ := os.ReadFile(s.Path())
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 5 { // header + user + assistant + toolResult + session_info
		t.Fatalf("expected 5 lines, got %d", len(lines))
	}

	var e map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &e); err != nil {
		t.Fatal(err)
	}
	// Babylon: entry.type === "message", entry.id, entry.message.role/content/timestamp
	if e["type"] != "message" {
		t.Errorf("line 1 type = %v", e["type"])
	}
	if e["id"] != user.ID {
		t.Errorf("entryId = %v, want %s", e["id"], user.ID)
	}
	msg, _ := e["message"].(map[string]any)
	if msg == nil || msg["role"] != "user" {
		t.Fatalf("bad message: %v", e["message"])
	}
	if _, ok := msg["timestamp"].(float64); !ok {
		t.Errorf("numeric timestamp missing: %v", msg["timestamp"])
	}

	var tr map[string]any
	json.Unmarshal([]byte(lines[3]), &tr)
	tmsg, _ := tr["message"].(map[string]any)
	if tmsg["role"] != "toolResult" || tmsg["toolCallId"] != "call_1" {
		t.Errorf("bad toolResult: %v", tmsg)
	}

	var si map[string]any
	json.Unmarshal([]byte(lines[4]), &si)
	if si["type"] != "session_info" || si["name"] != "Auto title" {
		t.Errorf("bad session_info: %v", si)
	}

	// The tool result must also be findable via the toolCallId scan that
	// Babylon's readToolOutput performs.
	found := false
	for _, line := range lines {
		if !strings.Contains(line, "call_1") {
			continue
		}
		var ent map[string]any
		if err := json.Unmarshal([]byte(line), &ent); err != nil {
			continue
		}
		if m, _ := ent["message"].(map[string]any); m != nil && m["toolCallId"] == "call_1" {
			found = true
		}
	}
	if !found {
		t.Error("readToolOutput-style scan failed to find toolCallId")
	}

	_ = result
	_ = info
}

func TestAppendChainParentIDs(t *testing.T) {
	s := newTestStore(t)
	e1, _ := s.Append(Entry{Type: TypeMessage, Message: &Message{Role: "user", Timestamp: 1}})
	e2, _ := s.Append(Entry{Type: TypeMessage, Message: &Message{Role: "assistant", Timestamp: 2}})
	if e2.ParentID != e1.ID {
		t.Errorf("parentId = %s, want %s", e2.ParentID, e1.ID)
	}
	if s.LastID() != e2.ID {
		t.Errorf("lastID = %s, want %s", s.LastID(), e2.ID)
	}
}

func TestReopenRecoversID(t *testing.T) {
	s := newTestStore(t)
	id := s.ID()
	s.Close()
	s2, err := Open(s.Path(), "/tmp/proj")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.ID() != id {
		t.Errorf("reopened id = %s, want %s", s2.ID(), id)
	}
}

func TestTailAndRange(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 50; i++ {
		_, err := s.Append(Entry{Type: TypeMessage, Message: &Message{Role: "user", Content: []Block{{Type: "text", Text: "msg"}}, Timestamp: int64(i)}})
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, start, err := Tail(s.Path(), 2048)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("tail returned no entries")
	}
	if start <= 0 {
		t.Fatalf("start offset = %d, want > 0", start)
	}
	// All tail entries must have increasing timestamps (i.e. we skipped the head).
	if entries[0].Message.Timestamp < 30 {
		t.Fatalf("tail should skip old entries, first ts = %d", entries[0].Message.Timestamp)
	}

	// Range back from the tail start: the next older window.
	older, _, err := Range(s.Path(), &start, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if len(older) == 0 {
		t.Fatal("older window empty")
	}
	if older[len(older)-1].Message.Timestamp >= entries[0].Message.Timestamp {
		t.Errorf("windows overlap: older last ts = %d, tail first ts = %d", older[len(older)-1].Message.Timestamp, entries[0].Message.Timestamp)
	}
}

// TestTailBackfill covers Babylon's window-extension behavior: a giant single
// line (multi-MB tool output) must not break the tail read.
func TestTailBackfill(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 10; i++ {
		_, _ = s.Append(Entry{Type: TypeMessage, Message: &Message{Role: "user", Timestamp: int64(i)}})
	}
	big := strings.Repeat("x", 4*1024*1024)
	_, err := s.Append(Entry{Type: TypeMessage, Message: &Message{Role: RoleToolResult, ToolCallID: "big", Content: []Block{{Type: "text", Text: big}}, Timestamp: 99}})
	if err != nil {
		t.Fatal(err)
	}
	entries, _, err := Tail(s.Path(), 8192)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("tail should backfill past the giant line")
	}
	last := entries[len(entries)-1]
	if last.Message == nil || last.Message.ToolCallID != "big" {
		t.Fatalf("expected giant tool result last, got %+v", last)
	}
}

func TestReadInfo(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append(Entry{Type: TypeMessage, Message: &Message{Role: RoleUser, Content: []Block{{Type: "text", Text: "  do   the   thing  "}}, Timestamp: 1}})
	_, _ = s.Append(Entry{Type: TypeMessage, Message: &Message{Role: RoleAssistant, Timestamp: 2}})
	_, _ = s.Append(Entry{Type: TypeSessionInfo, Name: "My title"})

	info, err := ReadInfo(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info == nil {
		t.Fatal("nil info")
	}
	if info.Name != "My title" {
		t.Errorf("name = %q", info.Name)
	}
	if !strings.Contains(info.FirstUserText, "do the thing") || strings.Contains(info.FirstUserText, "  ") {
		t.Errorf("firstUserText = %q", info.FirstUserText)
	}
	if info.Cwd != "/tmp/proj" || info.ID != s.ID() {
		t.Errorf("info = %+v", info)
	}
}

func TestList(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 3; i++ {
		path, err := NewPath(root, "/tmp/proj")
		if err != nil {
			t.Fatal(err)
		}
		s, err := Open(path, "/tmp/proj")
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	infos, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 {
		t.Fatalf("want 3 sessions, got %d", len(infos))
	}
	for i := 1; i < len(infos); i++ {
		if infos[i].Mtime > infos[i-1].Mtime {
			t.Error("sessions not sorted newest-first")
		}
	}
}

func TestSlug(t *testing.T) {
	if got := SlugForDir("/Users/x/Proj"); got != "--Users-x-Proj--" {
		t.Errorf("slug = %q", got)
	}
}

func TestNewPathShape(t *testing.T) {
	root := t.TempDir()
	p, err := NewPath(root, "/tmp/proj")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(p)
	if !strings.HasPrefix(base, "20") || !strings.Contains(base, "_") || !strings.HasSuffix(base, ".jsonl") {
		t.Errorf("filename shape wrong: %s", base)
	}
	if !strings.Contains(p, "--tmp-proj--") {
		t.Errorf("path missing slug: %s", p)
	}
}
