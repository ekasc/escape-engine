package agent

import (
	"github.com/ekasc/escape/engine/internal/provider"
	"github.com/ekasc/escape/engine/internal/session"
)

// historyFromStore rebuilds the provider message list from the session file,
// so every model call sees the full conversation. Thinking blocks are not
// sent back (most OpenAI-compatible APIs reject them as input).
//
// Compaction is respected: entries before the latest compaction entry's kept
// boundary are summarized away and their summary is folded into the system
// prompt, so the model call only carries the summary + the recent messages.
func historyFromStore(path, systemPrompt string) ([]provider.Message, error) {
	entries, err := session.ReadAll(path)
	if err != nil {
		return nil, err
	}

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
