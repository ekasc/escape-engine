package session

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// writeTestSession builds a session file with a fixed sequence of entries and
// returns its path.
func writeTestSession(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	st, err := Open(path, "/tmp/proj")
	if err != nil {
		t.Fatal(err)
	}
	appendEntry := func(e Entry) {
		t.Helper()
		if _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	appendEntry(Entry{Type: TypeSessionInfo, Name: "Test session"})
	appendEntry(Entry{Type: TypeMessage, Message: &Message{
		Role:      RoleUser,
		Content:   []Block{{Type: BlockText, Text: "hello"}},
		Timestamp: 1000,
	}})
	appendEntry(Entry{Type: TypeMessage, Message: &Message{
		Role:       RoleAssistant,
		Content:    []Block{{Type: BlockText, Text: "hi"}},
		Model:      "deepseek-v4-flash",
		StopReason: StopStop,
		Usage: &Usage{
			Input: 1000, Output: 200, CacheRead: 3000, CacheWrite: 500,
		},
		Timestamp: 2000,
	}})
	appendEntry(Entry{Type: TypeMessage, Message: &Message{
		Role:       RoleToolResult,
		ToolCallID: "c1",
		ToolName:   "bash",
		Content:    []Block{{Type: BlockText, Text: "out"}},
		Timestamp:  3000,
	}})
	appendEntry(Entry{Type: TypeMessage, Message: &Message{
		Role:       RoleAssistant,
		Content:    []Block{{Type: BlockToolCall, ID: "c2", Name: "read", Arguments: map[string]any{"path": "a.go"}}},
		Model:      "gpt-5.6-luna",
		StopReason: StopToolUse,
		Usage:      &Usage{Input: 5000, Output: 1000},
		Timestamp:  4000,
	}})
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStatsCountsAndTokens(t *testing.T) {
	path := writeTestSession(t)
	s, err := Stats(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.SessionFile != path {
		t.Errorf("SessionFile = %q, want %q", s.SessionFile, path)
	}
	if s.SessionID == "" {
		t.Error("SessionID empty")
	}
	if s.UserMessages != 1 || s.AssistantMessages != 2 || s.ToolCalls != 1 || s.ToolResults != 1 || s.TotalMessages != 4 {
		t.Errorf("counts = user %d assistant %d calls %d results %d total %d, want 1/2/1/1/4",
			s.UserMessages, s.AssistantMessages, s.ToolCalls, s.ToolResults, s.TotalMessages)
	}
	want := TokenTotals{Input: 6000, Output: 1200, CacheRead: 3000, CacheWrite: 500, Total: 10700}
	if s.Tokens != want {
		t.Errorf("Tokens = %+v, want %+v", s.Tokens, want)
	}
}

func TestStatsCost(t *testing.T) {
	path := writeTestSession(t)
	s, err := Stats(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	// deepseek-v4-flash: (1000+3000+500)*0.07/1e6 + 200*0.14/1e6
	// gpt-5.6-luna: 5000*0.2/1e6 + 1000*1.2/1e6
	want := (4500*0.07+200*0.14)/1e6 + (5000*0.2+1000*1.2)/1e6
	if math.Abs(s.Cost-want) > 1e-12 {
		t.Errorf("Cost = %v, want %v", s.Cost, want)
	}
}

func TestStatsCompactionUsagePricedAtOwnModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	st, err := Open(path, "/tmp/p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(Entry{Type: TypeCompaction, Summary: "s", ModelID: "gpt-5.6-sol", Usage: &Usage{Input: 50, Output: 25}}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	s, err := Stats(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	// gpt-5.6-sol: 50*5/1e6 + 25*30/1e6
	want := (50*5 + 25*30) / 1e6
	if math.Abs(s.Cost-want) > 1e-12 {
		t.Errorf("Cost = %v, want %v", s.Cost, want)
	}
	if s.Tokens.Total != 75 {
		t.Errorf("Total = %d, want 75", s.Tokens.Total)
	}
	if s.UserMessages != 0 || s.AssistantMessages != 0 || s.TotalMessages != 0 {
		t.Errorf("compaction counted as messages: %+v", s)
	}
}

func TestStatsContextUsage(t *testing.T) {
	path := writeTestSession(t)
	s, err := Stats(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.ContextUsage == nil {
		t.Fatal("ContextUsage nil")
	}
	// Window from last model (gpt-5.6-luna).
	if s.ContextUsage.ContextWindow != 272000 {
		t.Errorf("ContextWindow = %d, want 272000", s.ContextUsage.ContextWindow)
	}
	// Last valid assistant usage: the gpt-5.6-luna message (6000 context
	// tokens), no trailing entries.
	if s.ContextUsage.Tokens == nil || *s.ContextUsage.Tokens != 6000 {
		t.Errorf("Tokens = %v, want 6000", s.ContextUsage.Tokens)
	}
	if s.ContextUsage.Percent == nil || *s.ContextUsage.Percent != 2 {
		t.Errorf("Percent = %v, want 2", s.ContextUsage.Percent)
	}

	// Explicit window override wins.
	s2, err := Stats(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if s2.ContextUsage.ContextWindow != 1000 {
		t.Errorf("ContextWindow = %d, want 1000", s2.ContextUsage.ContextWindow)
	}
	if s2.ContextUsage.Percent == nil || *s2.ContextUsage.Percent != 600 {
		t.Errorf("Percent = %v, want 600", s2.ContextUsage.Percent)
	}
}

func TestStatsContextUsageNullAfterCompaction(t *testing.T) {
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
		Role: RoleUser, Content: []Block{{Type: BlockText, Text: "u1"}}, Timestamp: 1,
	}})
	appendE(Entry{Type: TypeMessage, Message: &Message{
		Role: RoleAssistant, Content: []Block{{Type: BlockText, Text: "a1"}}, Model: "minimax-m3",
		StopReason: StopStop, Usage: &Usage{Input: 900}, Timestamp: 2,
	}})
	appendE(Entry{Type: TypeCompaction, Summary: "sum", TokensBefore: 900, FirstKeptEntryID: "x1"})
	appendE(Entry{Type: TypeMessage, Message: &Message{
		Role: RoleUser, Content: []Block{{Type: BlockText, Text: "u2 after compaction"}}, Timestamp: 3,
	}})
	st.Close()

	s, err := Stats(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.ContextUsage == nil {
		t.Fatal("ContextUsage nil")
	}
	if s.ContextUsage.ContextWindow != 1000000 {
		t.Errorf("ContextWindow = %d, want 1000000", s.ContextUsage.ContextWindow)
	}
	if s.ContextUsage.Tokens != nil || s.ContextUsage.Percent != nil {
		t.Errorf("Tokens/Percent = %v/%v, want null after compaction without post-compaction assistant",
			s.ContextUsage.Tokens, s.ContextUsage.Percent)
	}
	if s.Tokens.Input != 900 {
		t.Errorf("Tokens.Input = %d, want 900 (compaction itself adds no usage)", s.Tokens.Input)
	}
}

func TestStatsPostCompactionAssistantRestoresContext(t *testing.T) {
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
		Role: RoleUser, Content: []Block{{Type: BlockText, Text: "u1"}}, Timestamp: 1,
	}})
	appendE(Entry{Type: TypeCompaction, Summary: "sum"})
	appendE(Entry{Type: TypeMessage, Message: &Message{
		Role: RoleAssistant, Content: []Block{{Type: BlockText, Text: "a2"}}, Model: "minimax-m3",
		StopReason: StopStop, Usage: &Usage{Input: 700, Output: 100}, Timestamp: 4,
	}})
	appendE(Entry{Type: TypeMessage, Message: &Message{
		Role: RoleUser, Content: []Block{{Type: BlockText, Text: "u2"}}, Timestamp: 5,
	}})
	st.Close()

	s, err := Stats(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Context = 800 (input+output of post-compaction assistant) + 1 ("u2" = 2 chars -> 1 token).
	if s.ContextUsage.Tokens == nil || *s.ContextUsage.Tokens != 801 {
		t.Errorf("Tokens = %v, want 801", s.ContextUsage.Tokens)
	}
}

func TestStatsIgnoresAbortedAssistantUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	st, err := Open(path, "/tmp/p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(Entry{Type: TypeMessage, Message: &Message{
		Role: RoleAssistant, Content: []Block{{Type: BlockText, Text: "partial"}},
		Model: "minimax-m3", StopReason: StopAborted, Usage: &Usage{Input: 500}, Timestamp: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	s, err := Stats(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Usage still counts toward totals, but not toward the context estimate.
	if s.Tokens.Input != 500 {
		t.Errorf("Tokens.Input = %d, want 500", s.Tokens.Input)
	}
	if s.ContextUsage.Tokens != nil {
		t.Errorf("Tokens = %v, want nil (aborted usage not valid for context)", s.ContextUsage.Tokens)
	}
	if s.AssistantMessages != 1 {
		t.Errorf("AssistantMessages = %d, want 1", s.AssistantMessages)
	}
}

func TestStatsNoHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Stats(path, 0); err == nil {
		t.Error("Stats on file without header: expected error")
	}
	if _, err := Stats(filepath.Join(t.TempDir(), "missing.jsonl"), 0); err == nil {
		t.Error("Stats on missing file: expected error")
	}
}

func TestLookupModel(t *testing.T) {
	cases := []struct {
		model string
		win   int
		in    float64
		out   float64
	}{
		{"deepseek-v4-flash", 1000000, 0.07, 0.14},
		{"deepseek-v4-pro", 1000000, 0.435, 0.87},
		{"minimax-m3", 1000000, 0.3, 1.2},
		{"minimax-m3.2", 200000, 0, 0},
		{"glm-5-7b", 1000000, 1.4, 4.4},
		{"glm-5", 1000000, 1.4, 4.4},
		{"gpt-5.6-luna", 272000, 0.2, 1.2},
		{"gpt-5.6-sol", 272000, 5, 30},
		{"gpt-5.6-terra", 272000, 2, 12},
		{"gpt-5.6-omega", 272000, 0, 0},
		{"GPT-5.6-LUNA", 272000, 0.2, 1.2},
		{"unknown-model", 200000, 0, 0},
		{"", 200000, 0, 0},
	}
	for _, c := range cases {
		m := LookupModel(c.model)
		if m.ContextWindow != c.win || m.InputPricePerMillion != c.in || m.OutputPricePerMillion != c.out {
			t.Errorf("LookupModel(%q) = %+v, want window %d in %v out %v", c.model, m, c.win, c.in, c.out)
		}
	}
}
