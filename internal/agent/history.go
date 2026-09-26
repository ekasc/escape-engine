package agent

import (
	"sync"

	"github.com/ekasc/escape-engine/internal/provider"
	"github.com/ekasc/escape-engine/internal/session"
)

// entryCache remembers how much of a session file has already been turned into
// entries. A session file is append-only, so re-reading it in full on every turn
// is wasted work that grows without bound: a long session reaches tens of
// megabytes and hundreds of milliseconds per turn before the provider is called
// at all.
type entryCache struct {
	// mu guards the cache. More than one goroutine reaches it: the turn, recap
	// while a turn is running, and priming when a session is opened.
	mu      sync.Mutex
	entries []session.Entry
	offset  int64
	path    string
}

// appendReadWindow bounds one read. A turn that appends more than this is
// covered by the loop rather than by a single unbounded read.
const appendReadWindow = 4 * 1024 * 1024

// sinceAppends returns every entry in the file, reading only the bytes appended
// since the previous call. A different path, or a file that shrank, means the
// cache describes a file that no longer exists, so it restarts from zero.
func (c *entryCache) sinceAppends(path string) ([]session.Entry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path != path {
		c.entries = nil
		c.offset = 0
		c.path = path
	}
	for {
		fresh, offset, err := session.Since(path, c.offset, appendReadWindow)
		if err != nil {
			return nil, err
		}
		if offset == c.offset {
			return c.entries, nil
		}
		c.entries = append(c.entries, fresh...)
		c.offset = offset
	}
}

func messagesFromEntries(entries []session.Entry, systemPrompt string) ([]provider.Message, error) {
	firstKeptIdx := 0
	var summary string
	var snapImages []*provider.Image
	var snapFallback string
	for i := range entries {
		e := entries[i]
		if e.Type != session.TypeCompaction || e.FirstKeptEntryID == "" {
			continue
		}
		for j := 0; j < len(entries); j++ {
			if entries[j].ID == e.FirstKeptEntryID {
				firstKeptIdx = j
			}
		}
		summary = e.Summary
		if e.SnapcompactData != "" {
			if images, fallback, err := snapArchiveImages(e.SnapcompactData); err == nil {
				snapImages, snapFallback = images, fallback
			}
		}
	}
	if summary != "" {
		systemPrompt += "\n\n<conversation_summary>\n" + summary + "\n</conversation_summary>\n"
	}

	msgs := []provider.Message{{Role: "system", Text: systemPrompt}}
	if len(snapImages) > 0 {
		text := "[Snapcompact archive]\n" + snapFallback
		msgs = append(msgs, provider.Message{Role: "user", Text: text, Images: snapImages})
	}
	for i := firstKeptIdx; i < len(entries); i++ {
		e := entries[i]
		if e.Type != session.TypeMessage || e.Message == nil {
			continue
		}
		m := e.Message
		switch m.Role {
		case session.RoleUser:
			msgs = append(msgs, provider.Message{Role: "user", Text: m.Text(false)})
		case session.RoleAssistant:
			pm := provider.Message{Role: "assistant", Text: m.Text(false)}
			for _, b := range m.Content {
				if b.Type == session.BlockToolCall {
					pm.ToolCalls = append(pm.ToolCalls, provider.ToolCall{ID: b.ID, Name: b.Name, Args: b.Arguments})
				}
			}
			msgs = append(msgs, pm)
		case session.RoleToolResult:
			msgs = append(msgs, provider.Message{
				Role:         "tool",
				ToolCallID:   m.ToolCallID,
				ToolCallName: m.ToolName,
				Text:         m.Text(false),
			})
		}
	}
	return msgs, nil
}

// prime reads a session into the cache without needing the result.
//
// The turn cannot begin until it has the whole transcript, and on a long
// session that read is the largest thing between a message and the provider.
// Opening a session is also when the transcript is read to display it, so
// starting the parse then overlaps it with the time the person spends reading,
// instead of charging it to their first message.
func (c *entryCache) prime(path string) {
	_, _ = c.sinceAppends(path)
}
