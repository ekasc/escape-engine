package agent

import (
	"github.com/ekasc/pi-go/internal/provider"
	"github.com/ekasc/pi-go/internal/session"
)

// historyFromStore rebuilds the provider message list from the session file,
// so every model call sees the full conversation. Thinking blocks are not
// sent back (most OpenAI-compatible APIs reject them as input).
func historyFromStore(path, systemPrompt string) ([]provider.Message, error) {
	entries, err := session.ReadAll(path)
	if err != nil {
		return nil, err
	}

	msgs := []provider.Message{{Role: "system", Text: systemPrompt}}
	for _, e := range entries {
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
