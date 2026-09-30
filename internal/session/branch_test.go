package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func appendMsg(t *testing.T, s *Store, role, text, parentID string) Entry {
	t.Helper()
	e, err := s.Append(Entry{
		Type:     TypeMessage,
		ParentID: parentID,
		Message: &Message{
			Role:      role,
			Content:   []Block{{Type: BlockText, Text: text}},
			Timestamp: NowMillis(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// buildBranched writes a session with two branches off the same assistant
// reply and returns the entries keyed by label:
//
//	a1(user) -> a2(assistant) -> b1(user) -> b2(assistant)
//	                          \-> c1(user) -> c2(assistant)  (leaf)
func buildBranched(t *testing.T) (string, map[string]Entry) {
	t.Helper()
	s := newTestStore(t)
	byLabel := map[string]Entry{}
	a1 := appendMsg(t, s, RoleUser, "first", "")
	byLabel["a1"] = a1
	a2 := appendMsg(t, s, RoleAssistant, "reply", a1.ID)
	byLabel["a2"] = a2
	b1 := appendMsg(t, s, RoleUser, "branch A", a2.ID)
	byLabel["b1"] = b1
	b2 := appendMsg(t, s, RoleAssistant, "A done", b1.ID)
	byLabel["b2"] = b2
	c1 := appendMsg(t, s, RoleUser, "branch B", a2.ID)
	byLabel["c1"] = c1
	c2 := appendMsg(t, s, RoleAssistant, "B done", c1.ID)
	byLabel["c2"] = c2
	return s.Path(), byLabel
}

func TestTreeLinearAndLeaf(t *testing.T) {
	path, byID := buildBranched(t)

	roots, leaf, err := Tree(path)
	if err != nil {
		t.Fatal(err)
	}
	if leaf != byID["c2"].ID {
		t.Errorf("leaf = %s, want c2", leaf)
	}
	if len(roots) != 1 {
		t.Fatalf("roots = %d, want 1 (header must not be a node)", len(roots))
	}
	root := roots[0]
	if root.Entry.ID != byID["a1"].ID {
		t.Fatalf("root = %s, want a1", root.Entry.ID)
	}
	if len(root.Children) != 1 || root.Children[0].Entry.ID != byID["a2"].ID {
		t.Fatalf("a1 children wrong: %+v", root.Children)
	}
	forkNode := root.Children[0]
	if len(forkNode.Children) != 2 {
		t.Fatalf("a2 children = %d, want 2 (branch A + branch B)", len(forkNode.Children))
	}
	// Children sorted oldest-first (file order here).
	if forkNode.Children[0].Entry.ID != byID["b1"].ID || forkNode.Children[1].Entry.ID != byID["c1"].ID {
		t.Errorf("children order wrong: %s, %s", forkNode.Children[0].Entry.ID, forkNode.Children[1].Entry.ID)
	}
	// Each branch has one assistant child.
	if forkNode.Children[0].Children[0].Entry.ID != byID["b2"].ID {
		t.Errorf("branch A child = %s, want b2", forkNode.Children[0].Children[0].Entry.ID)
	}
	if forkNode.Children[1].Children[0].Entry.ID != byID["c2"].ID {
		t.Errorf("branch B child = %s, want c2", forkNode.Children[1].Children[0].Entry.ID)
	}
}

func TestTreeSortsChildrenByTimestamp(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.Append(Entry{Type: TypeMessage, ParentID: "", Timestamp: "2024-01-01T00:00:00.000Z", Message: &Message{Role: "user", Content: []Block{{Type: "text", Text: "a"}}, Timestamp: 1}})
	x, _ := s.Append(Entry{Type: TypeMessage, ParentID: a.ID, Timestamp: "2024-01-01T00:00:03.000Z", Message: &Message{Role: "assistant", Content: []Block{{Type: "text", Text: "late"}}, Timestamp: 3}})
	y, _ := s.Append(Entry{Type: TypeMessage, ParentID: a.ID, Timestamp: "2024-01-01T00:00:01.000Z", Message: &Message{Role: "assistant", Content: []Block{{Type: "text", Text: "early"}}, Timestamp: 1}})
	_ = x

	roots, _, err := Tree(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	kids := roots[0].Children
	if len(kids) != 2 || kids[0].Entry.ID != y.ID || kids[1].Entry.ID != x.ID {
		t.Errorf("children not sorted by timestamp: got %s, %s; want %s, %s", kids[0].Entry.ID, kids[1].Entry.ID, y.ID, x.ID)
	}
}

func TestLeafID(t *testing.T) {
	path, byID := buildBranched(t)
	leaf, err := LeafID(path)
	if err != nil {
		t.Fatal(err)
	}
	if leaf != byID["c2"].ID {
		t.Errorf("leaf = %s, want c2", leaf)
	}

	// Only a header: no entries -> empty leaf. The header is written by hand
	// because a store no longer creates one on open; see store.go's Open.
	s := newTestStore(t)
	if _, err := s.Append(Entry{Type: TypeSession, ID: NewSessionID(), Version: 3, Cwd: "/tmp/proj"}); err != nil {
		t.Fatal(err)
	}
	leaf, err = LeafID(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if leaf != "" {
		t.Errorf("leaf = %q, want empty", leaf)
	}
}

func TestCommonAncestor(t *testing.T) {
	path, byID := buildBranched(t)

	cases := []struct {
		a, b string
		want string
	}{
		{byID["b2"].ID, byID["c2"].ID, byID["a2"].ID},
		{byID["b1"].ID, byID["b2"].ID, byID["b1"].ID},
		{byID["a1"].ID, byID["c2"].ID, byID["a1"].ID},
		{byID["c2"].ID, byID["c2"].ID, byID["c2"].ID}, // same id
	}
	for _, tc := range cases {
		got, err := CommonAncestor(path, tc.a, tc.b)
		if err != nil {
			t.Fatalf("CommonAncestor(%s, %s): %v", tc.a, tc.b, err)
		}
		if got != tc.want {
			t.Errorf("CommonAncestor(%s, %s) = %s, want %s", tc.a, tc.b, got, tc.want)
		}
	}

	// Missing id -> error.
	if _, err := CommonAncestor(path, byID["a1"].ID, "nope"); err == nil {
		t.Error("expected error for missing entry")
	}
	// Two orphan roots share no ancestor.
	s := newTestStore(t)
	o1, _ := s.Append(Entry{Type: TypeMessage, ParentID: "missing-1", Message: &Message{Role: "user", Timestamp: 1}})
	o2, _ := s.Append(Entry{Type: TypeMessage, ParentID: "missing-2", Message: &Message{Role: "user", Timestamp: 2}})
	got, err := CommonAncestor(s.Path(), o1.ID, o2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("disjoint orphans common ancestor = %s, want empty", got)
	}
}

func TestEntriesSince(t *testing.T) {
	path, byID := buildBranched(t)

	entries, leaf, err := EntriesSince(path, byID["a2"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if leaf != byID["c2"].ID {
		t.Errorf("leaf = %s, want c2", leaf)
	}
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	want := []string{byID["b1"].ID, byID["b2"].ID, byID["c1"].ID, byID["c2"].ID}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("entries since a2 = %v, want %v", ids, want)
	}

	// Since the leaf: empty result.
	entries, _, err = EntriesSince(path, byID["c2"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("entries since leaf = %d, want 0", len(entries))
	}

	// Unknown since id -> error.
	if _, _, err := EntriesSince(path, "nope"); err == nil {
		t.Error("expected error for unknown since id")
	}
}

func TestFork(t *testing.T) {
	path, byID := buildBranched(t)

	newPath, text, err := Fork(path, byID["b1"].ID, "/tmp/fork-cwd")
	if err != nil {
		t.Fatal(err)
	}
	if text != "branch A" {
		t.Errorf("text = %q, want %q", text, "branch A")
	}
	if newPath == path {
		t.Fatal("fork wrote into the source file")
	}

	entries, err := ReadAll(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 { // header + a1 + a2 + b1
		t.Fatalf("fork entries = %d, want 4", len(entries))
	}
	h := entries[0]
	if h.Type != TypeSession || h.Version != 3 || h.Cwd != "/tmp/fork-cwd" {
		t.Errorf("fork header wrong: %+v", h)
	}
	if h.ParentSession != path {
		t.Errorf("parentSession = %q, want %q", h.ParentSession, path)
	}
	if h.ID == "" || h.ID == byID["a1"].ID {
		t.Errorf("fork header id = %q, want a fresh session id", h.ID)
	}
	got := []string{entries[1].ID, entries[2].ID, entries[3].ID}
	want := []string{byID["a1"].ID, byID["a2"].ID, byID["b1"].ID}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("forked chain = %v, want %v", got, want)
	}
	// parentId chain preserved inside the new file.
	if entries[2].ParentID != byID["a1"].ID || entries[3].ParentID != byID["a2"].ID {
		t.Errorf("parent chain not preserved: %+v %+v", entries[2], entries[3])
	}

	// Fork on a non-user entry: text is empty.
	_, text, err = Fork(path, byID["b2"].ID, "/tmp/fork-cwd")
	if err != nil {
		t.Fatal(err)
	}
	if text != "" {
		t.Errorf("non-user fork text = %q, want empty", text)
	}

	// Unknown entry -> error.
	if _, _, err := Fork(path, "nope", "/tmp/fork-cwd"); err == nil {
		t.Error("expected error for unknown fork entry")
	}
}

func TestClone(t *testing.T) {
	path, byID := buildBranched(t)

	newPath, err := Clone(path, "/tmp/clone-cwd")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ReadAll(newPath)
	if err != nil {
		t.Fatal(err)
	}
	// Active branch is root->a1->a2->c1->c2; branch A entries are excluded.
	if len(entries) != 5 { // header + 4 entries
		t.Fatalf("clone entries = %d, want 5", len(entries))
	}
	h := entries[0]
	if h.ParentSession != path || h.Cwd != "/tmp/clone-cwd" {
		t.Errorf("clone header wrong: %+v", h)
	}
	got := []string{entries[1].ID, entries[2].ID, entries[3].ID, entries[4].ID}
	want := []string{byID["a1"].ID, byID["a2"].ID, byID["c1"].ID, byID["c2"].ID}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("cloned branch = %v, want %v", got, want)
	}

	// Empty session (header only) -> error.
	s := newTestStore(t)
	if _, err := Clone(s.Path(), "/tmp/clone-cwd"); err == nil {
		t.Error("expected error cloning empty session")
	}
}

func TestLastAssistantText(t *testing.T) {
	path, byID := buildBranched(t)
	text, err := LastAssistantText(path)
	if err != nil {
		t.Fatal(err)
	}
	if text != "B done" {
		t.Errorf("text = %q, want %q", text, "B done")
	}
	_ = byID

	// Aborted assistant with no content is skipped.
	s := newTestStore(t)
	u, _ := s.Append(Entry{Type: TypeMessage, Message: &Message{Role: RoleUser, Content: []Block{{Type: "text", Text: "hi"}}, Timestamp: 1}})
	_, _ = s.Append(Entry{Type: TypeMessage, ParentID: u.ID, Message: &Message{Role: RoleAssistant, StopReason: StopAborted, Timestamp: 2}})
	ok, _ := s.Append(Entry{Type: TypeMessage, ParentID: u.ID, Message: &Message{Role: RoleAssistant, Content: []Block{{Type: "text", Text: "  done  "}}, Timestamp: 3}})
	text, err = LastAssistantText(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if text != "done" {
		t.Errorf("text = %q, want %q (trimmed)", text, "done")
	}
	_ = ok

	// No assistant message at all -> empty.
	s2 := newTestStore(t)
	_, _ = s2.Append(Entry{Type: TypeMessage, Message: &Message{Role: RoleUser, Content: []Block{{Type: "text", Text: "hi"}}, Timestamp: 1}})
	text, err = LastAssistantText(s2.Path())
	if err != nil {
		t.Fatal(err)
	}
	if text != "" {
		t.Errorf("text = %q, want empty", text)
	}
}

func TestSetName(t *testing.T) {
	s := newTestStore(t)
	path := s.Path()
	if err := SetName(s, "Refactor auth"); err != nil {
		t.Fatal(err)
	}
	name, err := Name(path)
	if err != nil {
		t.Fatal(err)
	}
	if name != "Refactor auth" {
		t.Errorf("name = %q, want %q", name, "Refactor auth")
	}
	// Appends, never rewrites: header + new session_info line only.
	entries, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].Type != TypeSessionInfo || entries[1].Name != "Refactor auth" {
		t.Errorf("setName append wrong: %+v", entries)
	}
	if entries[1].ParentID == "" {
		t.Errorf("session_info parentId = %q", entries[1].ParentID)
	}
}

func TestGetForkMessages(t *testing.T) {
	path, byID := buildBranched(t)
	msgs, err := GetForkMessages(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("fork messages = %d, want 3", len(msgs))
	}
	want := []string{byID["a1"].ID, byID["b1"].ID, byID["c1"].ID}
	for i, m := range msgs {
		if m.EntryID != want[i] {
			t.Errorf("msgs[%d].EntryID = %s, want %s", i, m.EntryID, want[i])
		}
	}
	if msgs[0].Text != "first" || msgs[1].Text != "branch A" || msgs[2].Text != "branch B" {
		t.Errorf("texts wrong: %+v", msgs)
	}

	// User messages with only whitespace are skipped.
	s := newTestStore(t)
	_, _ = s.Append(Entry{Type: TypeMessage, Message: &Message{Role: RoleUser, Content: []Block{{Type: "text", Text: "   "}}, Timestamp: 1}})
	_, _ = s.Append(Entry{Type: TypeMessage, Message: &Message{Role: RoleUser, Content: []Block{{Type: "text", Text: "real"}}, Timestamp: 2}})
	msgs, err = GetForkMessages(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != "real" {
		t.Errorf("whitespace filter wrong: %+v", msgs)
	}
}

// TestPiEntryShape locks the new entry types to pi's on-disk JSON: flat
// summary/firstKeptEntryId/tokensBefore/fromId/details/usage fields, image
// blocks as {type:"image", data, mimeType}, and no nesting.
func TestPiEntryShape(t *testing.T) {
	s := newTestStore(t)
	comp, err := s.Append(Entry{
		Type:             TypeCompaction,
		Summary:          "User discussed X, Y, Z",
		FirstKeptEntryID: "c3d4e5f6",
		TokensBefore:     50000,
		Details:          map[string]any{"readFiles": []string{"a.go"}},
		Usage:            &Usage{Input: 100, Output: 50, CacheRead: 10, CacheWrite: 5, TotalTokens: 165},
	})
	if err != nil {
		t.Fatal(err)
	}
	bs, err := s.Append(Entry{
		Type:    TypeBranchSummary,
		Summary: "Branch explored approach A",
		FromID:  comp.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = bs
	cm, err := s.Append(Entry{
		Type: TypeCustom,
		ID:   "custom1",
	})
	if err != nil {
		t.Fatal(err)
	}
	img, err := s.Append(Entry{
		Type: TypeMessage,
		Message: &Message{
			Role:      RoleUser,
			Content:   []Block{{Type: BlockImage, Data: "aGVsbG8=", MimeType: "image/png", Source: "/tmp/x.png"}},
			Timestamp: 1784702694866,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = cm
	_ = img

	lines := readLines(t, s.Path())
	if len(lines) != 5 { // header + compaction + branch_summary + custom_message + image msg
		t.Fatalf("lines = %d, want 5", len(lines))
	}

	var compObj map[string]any
	json.Unmarshal([]byte(lines[1]), &compObj)
	if compObj["type"] != "compaction" || compObj["summary"] != "User discussed X, Y, Z" ||
		compObj["firstKeptEntryId"] != "c3d4e5f6" || compObj["tokensBefore"] != float64(50000) {
		t.Errorf("compaction shape wrong: %v", compObj)
	}
	if _, nested := compObj["compaction"]; nested {
		t.Error("compaction fields must be flat, not nested")
	}
	usage, _ := compObj["usage"].(map[string]any)
	if usage == nil || usage["input"] != float64(100) || usage["totalTokens"] != float64(165) {
		t.Errorf("compaction usage wrong: %v", compObj["usage"])
	}
	details, _ := compObj["details"].(map[string]any)
	if details == nil {
		t.Errorf("compaction details missing: %v", compObj)
	}

	var bsObj map[string]any
	json.Unmarshal([]byte(lines[2]), &bsObj)
	if bsObj["type"] != "branch_summary" || bsObj["summary"] != "Branch explored approach A" || bsObj["fromId"] != comp.ID {
		t.Errorf("branch_summary shape wrong: %v", bsObj)
	}

	var cmObj map[string]any
	json.Unmarshal([]byte(lines[3]), &cmObj)
	if cmObj["type"] != "custom_message" || cmObj["id"] != "custom1" {
		t.Errorf("custom_message shape wrong: %v", cmObj)
	}

	var imgObj map[string]any
	json.Unmarshal([]byte(lines[4]), &imgObj)
	msg, _ := imgObj["message"].(map[string]any)
	content, _ := msg["content"].([]any)
	blk, _ := content[0].(map[string]any)
	if blk["type"] != "image" || blk["data"] != "aGVsbG8=" || blk["mimeType"] != "image/png" {
		t.Errorf("image block wrong: %v", blk)
	}
}

// TestParsePiSession round-trips a hand-written pi session containing the new
// entry types, exactly as pi would write them.
func TestParsePiSession(t *testing.T) {
	fixture := `{"type":"session","version":3,"id":"11111111-2222-4333-8444-555555555555","timestamp":"2024-12-03T14:00:00.000Z","cwd":"/tmp/x"}
{"type":"message","id":"a1b2c3d4","parentId":null,"timestamp":"2024-12-03T14:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"Hello"}]}}
{"type":"message","id":"b2c3d4e5","parentId":"a1b2c3d4","timestamp":"2024-12-03T14:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Hi!"}],"stopReason":"stop"}}
{"type":"compaction","id":"f6g7h8i9","parentId":"b2c3d4e5","timestamp":"2024-12-03T14:10:00.000Z","summary":"User discussed X, Y, Z...","firstKeptEntryId":"c3d4e5f6","tokensBefore":50000}
{"type":"branch_summary","id":"g7h8i9j0","parentId":"a1b2c3d4","timestamp":"2024-12-03T14:15:00.000Z","fromId":"f6g7h8i9","summary":"Branch explored approach A..."}
{"type":"custom_message","id":"i9j0k1l2","parentId":"g7h8i9j0","timestamp":"2024-12-03T14:25:00.000Z","customType":"my-extension","content":"Injected context...","display":true}
{"type":"message","id":"d3e4f5a6","parentId":"a1b2c3d4","timestamp":"2024-12-03T14:30:00.000Z","message":{"role":"user","content":[{"type":"image","data":"aGVsbG8=","mimeType":"image/png"}]}}
`
	path := filepath.Join(t.TempDir(), "pi-session.jsonl")
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 7 { // header + 6 entries
		t.Fatalf("entries = %d, want 7", len(entries))
	}

	comp := entries[3]
	if comp.Type != TypeCompaction || comp.Summary != "User discussed X, Y, Z..." ||
		comp.FirstKeptEntryID != "c3d4e5f6" || comp.TokensBefore != 50000 {
		t.Errorf("compaction parse wrong: %+v", comp)
	}
	bs := entries[4]
	if bs.Type != TypeBranchSummary || bs.Summary != "Branch explored approach A..." || bs.FromID != "f6g7h8i9" {
		t.Errorf("branch_summary parse wrong: %+v", bs)
	}
	custom := entries[5]
	if custom.Type != TypeCustom {
		t.Errorf("custom parse wrong: %+v", custom)
	}
	imgMsg := entries[6].Message
	if imgMsg == nil || len(imgMsg.Content) == 0 || imgMsg.Content[0].Type != BlockImage ||
		imgMsg.Content[0].Data != "aGVsbG8=" || imgMsg.Content[0].MimeType != "image/png" {
		t.Errorf("image parse wrong: %+v", imgMsg)
	}

	// Tree over the pi file: branch_summary and compaction hang off the chain.
	roots, leaf, err := Tree(path)
	if err != nil {
		t.Fatal(err)
	}
	if leaf != "d3e4f5a6" {
		t.Errorf("leaf = %s, want d3e4f5a6", leaf)
	}
	if len(roots) != 1 || roots[0].Entry.ID != "a1b2c3d4" {
		t.Fatalf("pi file roots wrong: %+v", roots)
	}
	// a1b2c3d4 has three children: the assistant reply, the branch_summary
	// (attached at the fork point, per pi's docs example) and the image user
	// message; b2c3d4e5 -> compaction; branch_summary -> custom_message.
	kids := roots[0].Children
	if len(kids) != 3 {
		t.Fatalf("children = %d, want 3", len(kids))
	}
	if kids[0].Entry.ID != "b2c3d4e5" {
		t.Errorf("first child = %s, want b2c3d4e5", kids[0].Entry.ID)
	}
	if kids[1].Entry.ID != "g7h8i9j0" || kids[2].Entry.ID != "d3e4f5a6" {
		t.Errorf("children order wrong: %s, %s", kids[1].Entry.ID, kids[2].Entry.ID)
	}
	if comp := kids[0].Children; len(comp) != 1 || comp[0].Entry.ID != "f6g7h8i9" {
		t.Errorf("assistant reply child = %+v, want compaction f6g7h8i9", comp)
	}
	if bsKids := kids[1].Children; len(bsKids) != 1 || bsKids[0].Entry.ID != "i9j0k1l2" {
		t.Errorf("branch_summary child = %+v, want custom_message i9j0k1l2", bsKids)
	}

	// Re-marshal: flat pi shape round-trips (no nesting introduced).
	raw, err := comp.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	if _, nested := obj["compaction"]; nested {
		t.Error("re-marshal introduced nested compaction object")
	}
	if obj["summary"] != "User discussed X, Y, Z..." || obj["tokensBefore"] != float64(50000) {
		t.Errorf("re-marshal lost flat fields: %v", obj)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}
