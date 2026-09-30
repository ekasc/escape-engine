package session

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// This file implements pi's session tree/branching model on top of the
// existing append-only JSONL store. Entries already carry id/parentId, so the
// tree is derived from the parentId chain; the active position (leaf) is the
// last entry in file order, mirroring pi's reload behavior.

// TreeNode is one node of the session tree: an entry plus its children in
// timestamp order (oldest first), matching pi's getTree().
type TreeNode struct {
	Entry    Entry
	Children []*TreeNode
}

// ForkMessage is a user message available as a fork source, as returned by
// GetForkMessages (pi's getUserMessagesForForking).
type ForkMessage struct {
	EntryID string `json:"entryId"`
	Text    string `json:"text"`
}

// ErrEntryNotFound is returned when an entry id does not exist in a session.
var ErrEntryNotFound = errors.New("session entry not found")

// Tree builds the full session tree from the parentId chain. The header entry
// is excluded; entries whose parent is missing (or themselves) become roots,
// exactly like pi's getTree(). Returns the roots and the leaf id (the last
// entry in file order, "" when the file has no entries).
func Tree(path string) ([]*TreeNode, string, error) {
	entries, err := ReadAll(path)
	if err != nil {
		return nil, "", err
	}

	nodes := make(map[string]*TreeNode, len(entries))
	order := make([]string, 0, len(entries))
	var leafID string
	for _, e := range entries {
		if e.Type == TypeSession || e.ID == "" {
			continue
		}
		nodes[e.ID] = &TreeNode{Entry: e}
		order = append(order, e.ID)
		leafID = e.ID
	}

	var roots []*TreeNode
	for _, id := range order {
		n := nodes[id]
		p := n.Entry.ParentID
		if p == "" || p == id {
			roots = append(roots, n)
			continue
		}
		parent, ok := nodes[p]
		if !ok {
			// Orphan (e.g. parent is the header id): treat as root.
			roots = append(roots, n)
			continue
		}
		parent.Children = append(parent.Children, n)
	}

	// Children are sorted oldest-first by entry timestamp, like pi's getTree.
	for _, n := range nodes {
		sortNodes(n.Children)
	}
	return roots, leafID, nil
}

// LeafID returns the id of the active leaf: the last entry in file order
// ("" if the session has no entries).
func LeafID(path string) (string, error) {
	entries, err := ReadAll(path)
	if err != nil {
		return "", err
	}
	var leaf string
	for _, e := range entries {
		if e.Type == TypeSession || e.ID == "" {
			continue
		}
		leaf = e.ID
	}
	return leaf, nil
}

// CommonAncestor returns the id of the deepest entry that lies on both the
// path of a and the path of b (the root-first common ancestor). Returns ""
// when the two entries share no ancestor. Both ids must exist.
func CommonAncestor(path, a, b string) (string, error) {
	entries, err := ReadAll(path)
	if err != nil {
		return "", err
	}
	parents := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Type == TypeSession || e.ID == "" {
			continue
		}
		parents[e.ID] = e.ParentID
	}
	if _, ok := parents[a]; !ok {
		return "", ErrEntryNotFound
	}
	if _, ok := parents[b]; !ok {
		return "", ErrEntryNotFound
	}

	onA := make(map[string]bool)
	for cur := a; cur != ""; {
		onA[cur] = true
		next, ok := parents[cur]
		if !ok {
			break
		}
		cur = next
	}

	// Walk b upward, deepest first: first id also on a's path is the
	// common ancestor (pi's collectEntriesForBranchSummary).
	for cur := b; cur != ""; {
		if onA[cur] {
			return cur, nil
		}
		next, ok := parents[cur]
		if !ok {
			break
		}
		cur = next
	}
	return "", nil
}

// EntriesSince returns all non-header entries after sinceID in append (file)
// order, plus the current leaf id. sinceID must exist.
func EntriesSince(path, sinceID string) ([]Entry, string, error) {
	entries, err := ReadAll(path)
	if err != nil {
		return nil, "", err
	}
	var leaf string
	idx := -1
	if sinceID == "" {
		idx = 0 // no cursor: return all entries from the start (pi get_entries)
	}
	for i, e := range entries {
		if e.Type == TypeSession {
			continue
		}
		if e.ID == sinceID {
			idx = i
		}
		leaf = e.ID
	}
	if idx == -1 {
		return nil, "", ErrEntryNotFound
	}
	var out []Entry
	for _, e := range entries[idx+1:] {
		if e.Type == TypeSession {
			continue
		}
		out = append(out, e)
	}
	return out, leaf, nil
}

// Fork creates a new session file containing the branch path from the root
// down to entryID (inclusive), preserving entry ids and parentIds. The new
// file gets a fresh header (new id, cwd, parentSession pointing at the
// source) under DefaultRoot(). text is the user message text of entryID (for
// prefilling an editor / auto-naming), or "" when entryID is not a user
// message.
func Fork(srcPath, entryID, cwd string) (newPath, text string, err error) {
	return ForkToRoot(srcPath, entryID, cwd, DefaultRoot())
}

// ForkToRoot is Fork with an explicit destination session root.
func ForkToRoot(srcPath, entryID, cwd, root string) (newPath, text string, err error) {
	path, err := branchPath(srcPath, entryID)
	if err != nil {
		return "", "", err
	}
	if len(path) == 0 {
		return "", "", ErrEntryNotFound
	}
	if e := path[len(path)-1]; e.Message != nil && e.Message.Role == RoleUser {
		text = strings.TrimSpace(e.Message.Text(false))
	}
	newPath, err = writeBranch(srcPath, cwd, root, path)
	if err != nil {
		return "", "", err
	}
	return newPath, text, nil
}

// Clone copies the active branch (root down to the leaf) into a new session
// file, with a fresh header (new id, cwd, parentSession pointing at the
// source) under DefaultRoot().
func Clone(path, cwd string) (newPath string, err error) {
	leaf, err := LeafID(path)
	if err != nil {
		return "", err
	}
	if leaf == "" {
		return "", ErrEntryNotFound
	}
	branch, err := branchPath(path, leaf)
	if err != nil {
		return "", err
	}
	return writeBranch(path, cwd, DefaultRoot(), branch)
}

// LastAssistantText returns the trimmed text of the most recent assistant
// message on the active branch ("" when there is none). Aborted assistant
// messages with no content are skipped, matching pi's getLastAssistantText.
func LastAssistantText(path string) (string, error) {
	entries, err := ReadAll(path)
	if err != nil {
		return "", err
	}
	byID := make(map[string]Entry, len(entries))
	for _, e := range entries {
		if e.Type == TypeSession || e.ID == "" {
			continue
		}
		byID[e.ID] = e
	}
	leaf, err := LeafID(path)
	if err != nil {
		return "", err
	}
	for cur := leaf; cur != ""; {
		e, ok := byID[cur]
		if !ok {
			break
		}
		if e.Message != nil && e.Message.Role == RoleAssistant &&
			!(e.Message.StopReason == StopAborted && len(e.Message.Content) == 0) {
			return strings.TrimSpace(e.Message.Text(false)), nil
		}
		cur = e.ParentID
	}
	return "", nil
}

// SetName appends a session_info entry with the given display name.
//
// It takes the store rather than a path because a store knows its own cwd, and
// writing the name must never be what brings a session into existence or
// decides which project it belongs to. This used to reopen the file by path
// with an empty cwd, which was harmless while opening a store always created
// the file with a header; once opening stopped creating anything, naming a
// session that had not been sent to yet wrote a header with no cwd, and every
// project-scoped listing then filtered that session out permanently.
func SetName(s *Store, name string) error {
	if s == nil {
		return errors.New("cannot name a session without a store")
	}
	_, err := s.Append(Entry{Type: TypeSessionInfo, Name: name})
	return err
}

// GetForkMessages returns one ForkMessage per user message with non-empty
// text, in file order, across all branches (pi's getUserMessagesForForking).
func GetForkMessages(path string) ([]ForkMessage, error) {
	entries, err := ReadAll(path)
	if err != nil {
		return nil, err
	}
	var out []ForkMessage
	for _, e := range entries {
		if e.Type != TypeMessage || e.Message == nil || e.Message.Role != RoleUser {
			continue
		}
		text := e.Message.Text(false)
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, ForkMessage{EntryID: e.ID, Text: text})
	}
	return out, nil
}

// --- helpers ---

// branchPath returns the root-first path of entries leading to entryID
// (inclusive). Walking stops when the parent chain exits the entry index
// (i.e. reaches the header id or a missing parent).
func branchPath(path, entryID string) ([]Entry, error) {
	entries, err := ReadAll(path)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Entry, len(entries))
	for _, e := range entries {
		if e.Type == TypeSession || e.ID == "" {
			continue
		}
		byID[e.ID] = e
	}
	if _, ok := byID[entryID]; !ok {
		return nil, ErrEntryNotFound
	}

	var rev []Entry
	for cur := entryID; cur != ""; {
		e, ok := byID[cur]
		if !ok {
			break
		}
		rev = append(rev, e)
		cur = e.ParentID
	}
	out := make([]Entry, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		out = append(out, rev[i])
	}
	return out, nil
}

// writeBranch creates a new session file under DefaultRoot() for cwd, writes
// a fresh header (parentSession = srcPath), then appends the branch entries
// verbatim (ids, timestamps and parentIds preserved).
func writeBranch(srcPath, cwd, root string, branch []Entry) (string, error) {
	newPath, err := NewPath(root, cwd)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(newPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()

	abs, err := filepath.Abs(srcPath)
	if err != nil {
		abs = srcPath
	}
	header := Entry{
		Type:          TypeSession,
		Version:       3,
		ID:            NewSessionID(),
		Timestamp:     NowISO(),
		Cwd:           cwd,
		ParentSession: abs,
	}
	w := bufio.NewWriter(f)
	line, err := header.Marshal()
	if err != nil {
		return "", err
	}
	if _, err := w.Write(append(line, '\n')); err != nil {
		return "", err
	}
	for _, e := range branch {
		line, err := e.Marshal()
		if err != nil {
			return "", err
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return "", err
		}
	}
	if err := w.Flush(); err != nil {
		return "", err
	}
	return newPath, nil
}

func sortNodes(nodes []*TreeNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		return nodes[i].Entry.Timestamp < nodes[j].Entry.Timestamp
	})
}
