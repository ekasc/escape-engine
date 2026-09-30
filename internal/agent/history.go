package agent

import (
	"strings"
	"sync"

	"github.com/ekasc/escape-engine/internal/provider"
	"github.com/ekasc/escape-engine/internal/session"
	"github.com/ekasc/escape-engine/internal/tools"
)

// imageNote is the text a message carries when its image was attached, and the
// prefix of the text it carries when the image could not be. Both come from the
// read tool's marker so the two cannot drift apart.
const imageNote = "Read image"

// imageSurrogate is appended when the model does not accept image input. The
// model is told the image is missing rather than sent base64 it cannot read, so
// it can report the gap instead of describing a picture it never saw.
const imageSurrogate = " (not shown: this model does not accept image input)"

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

// messagesFromEntries builds the provider transcript. model decides whether an
// image is attached or replaced by a text surrogate; it is the only reason this
// function needs to know which model is about to be called.
func messagesFromEntries(entries []session.Entry, systemPrompt, model string) ([]provider.Message, error) {
	seesImages := session.LookupModel(model).AcceptsImage()
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
		if seesImages {
			msgs = append(msgs, provider.Message{Role: "user", Text: "[Snapcompact archive]\n" + snapFallback, Images: snapImages})
		} else {
			msgs = append(msgs, provider.Message{Role: "user", Text: "[Snapcompact archive]" + imageSurrogate + "\n" + snapFallback})
		}
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
			pm := provider.Message{
				Role:         "tool",
				ToolCallID:   m.ToolCallID,
				ToolCallName: m.ToolName,
				Text:         m.Text(false),
			}
			if img, note, ok := readImage(pm.Text); ok {
				pm.Text = note
				if seesImages {
					pm.Image = img
				} else {
					pm.Text += imageSurrogate
				}
			}
			msgs = append(msgs, pm)
		}
	}
	return msgs, nil
}

// readImage recognises a tool result the read tool marked as an image and
// returns the decoded image with the text that should accompany it. A result
// that carries the marker but does not decode is not an image: the base64 was
// truncated or corrupted somewhere upstream, and reporting that as text is
// better than dropping the result.
func readImage(text string) (*provider.Image, string, bool) {
	uri, ok := strings.CutPrefix(text, tools.ImageOutputPrefix)
	if !ok {
		return nil, "", false
	}
	img, err := provider.ParseDataURI(uri)
	if err != nil {
		return nil, text, false
	}
	return img, imageNote, true
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
