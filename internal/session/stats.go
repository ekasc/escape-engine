package session

import (
	"encoding/json"
	"math"
)

// TokenTotals is the aggregated token accounting of a session. `total` is the
// sum of the components, mirroring pi's get_session_stats.
type TokenTotals struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	Total      int `json:"total"`
}

// ContextUsage estimates how much of the model context window the session is
// currently using. Tokens and Percent are null immediately after a
// compaction, until a fresh post-compaction assistant response provides valid
// usage data (matching pi's get_session_stats semantics).
type ContextUsage struct {
	Tokens        *int `json:"tokens"`
	ContextWindow int  `json:"contextWindow"`
	Percent       *int `json:"percent"`
}

// SessionStats is the session introspection payload (pi rpc get_session_stats).
// Named SessionStats because the package-level constructor Stats shares the
// identifier space.
type SessionStats struct {
	SessionFile       string        `json:"sessionFile"`
	SessionID         string        `json:"sessionId"`
	UserMessages      int           `json:"userMessages"`
	AssistantMessages int           `json:"assistantMessages"`
	ToolCalls         int           `json:"toolCalls"`
	ToolResults       int           `json:"toolResults"`
	TotalMessages     int           `json:"totalMessages"`
	Tokens            TokenTotals   `json:"tokens"`
	Cost              float64       `json:"cost"`
	ContextUsage      *ContextUsage `json:"contextUsage,omitempty"`
}

// Stats aggregates token usage and cost over ALL entries of a session file
// (including history that was compacted away), so totals reflect everything
// billed across the session.
//
// Tokens are summed from message.usage (assistant messages, tool results) and
// compaction/branch_summary entry usage. Cost is estimated per usage record as
// tokens x per-model price (modelmeta.go): input+cacheRead+cacheWrite at the
// input price, output at the output price. Tool and summary usage is priced
// at the session's last known model.
//
// ctxWindow overrides the context window used for contextUsage (from
// get_session_stats RPC or caller knowledge); when <= 0 the window comes from
// the session's last known model. contextUsage is nil when no window is
// available.
func Stats(path string, ctxWindow int) (*SessionStats, error) {
	entries, err := ReadAll(path)
	if err != nil {
		return nil, err
	}

	var sessionID string
	for _, e := range entries {
		if e.Type == TypeSession && e.ID != "" {
			sessionID = e.ID
			break
		}
	}
	if sessionID == "" {
		return nil, ErrEmpty
	}

	s := &SessionStats{SessionFile: path, SessionID: sessionID}

	// Last known model across the session (assistant message model, else the
	// entry-level modelId/model fields) — used for contextUsage and for
	// pricing usage records that don't carry a model of their own.
	lastModel := ""
	for _, e := range entries {
		if e.Message != nil && e.Message.Role == RoleAssistant && e.Message.Model != "" {
			lastModel = e.Message.Model
		} else if e.ModelID != "" {
			lastModel = e.ModelID
		} else if e.Model != "" {
			lastModel = e.Model
		}
	}

	// Per-record model for cost attribution.
	modelFor := func(e Entry) string {
		switch {
		case e.Message != nil && e.Message.Model != "":
			return e.Message.Model
		case e.ModelID != "":
			return e.ModelID
		case e.Model != "":
			return e.Model
		default:
			return lastModel
		}
	}
	addUsage := func(e Entry, u *Usage) {
		if u == nil {
			return
		}
		s.Tokens.Input += u.Input
		s.Tokens.Output += u.Output
		s.Tokens.CacheRead += u.CacheRead
		s.Tokens.CacheWrite += u.CacheWrite
		meta := LookupModel(modelFor(e))
		s.Cost += float64(u.Input+u.CacheRead+u.CacheWrite) * meta.InputPricePerMillion / 1e6
		s.Cost += float64(u.Output) * meta.OutputPricePerMillion / 1e6
	}

	for _, e := range entries {
		if e.Type == TypeCompaction || e.Type == TypeBranchSummary {
			addUsage(e, e.Usage)
			continue
		}
		if e.Type != TypeMessage || e.Message == nil {
			continue
		}
		s.TotalMessages++
		msg := e.Message
		switch msg.Role {
		case RoleUser:
			s.UserMessages++
		case RoleAssistant:
			s.AssistantMessages++
			s.ToolCalls += len(msg.ToolCalls())
			addUsage(e, msg.Usage)
		case RoleToolResult:
			s.ToolResults++
			addUsage(e, msg.Usage)
		default:
			// bashExecution and any other roles count as messages only.
		}
	}
	s.Tokens.Total = s.Tokens.Input + s.Tokens.Output + s.Tokens.CacheRead + s.Tokens.CacheWrite
	s.ContextUsage = contextUsage(entries, lastModel, ctxWindow)
	return s, nil
}

// contextUsage mirrors pi's getContextUsage: the window comes from ctxWindow
// (when > 0) or the last known model; the token estimate is the context tokens
// of the last valid assistant usage plus a chars/4 estimate of the messages
// after it. After the latest compaction, tokens/percent stay null until an
// assistant response with valid usage appears after the cut.
func contextUsage(entries []Entry, lastModel string, ctxWindow int) *ContextUsage {
	window := ctxWindow
	if window <= 0 {
		window = LookupModel(lastModel).ContextWindow
	}
	if window <= 0 {
		return nil
	}
	cu := &ContextUsage{ContextWindow: window}

	latestCompaction := -1
	for i := range entries {
		if entries[i].Type == TypeCompaction {
			latestCompaction = i
		}
	}

	// Find the last valid assistant usage (skipping aborted/error and
	// all-zero usage, which carry no usable data).
	lastUsage := -1
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Type != TypeMessage || e.Message == nil || e.Message.Role != RoleAssistant {
			continue
		}
		if validAssistantUsage(e.Message) {
			lastUsage = i
			break
		}
	}

	if latestCompaction >= 0 {
		// Only trust usage from an assistant that responded after the latest
		// compaction; otherwise the context size is unknown until the next
		// LLM response.
		hasPostCompaction := false
		for i := len(entries) - 1; i > latestCompaction; i-- {
			e := entries[i]
			if e.Type == TypeMessage && e.Message != nil && e.Message.Role == RoleAssistant && validAssistantUsage(e.Message) {
				hasPostCompaction = true
				break
			}
		}
		if !hasPostCompaction {
			return cu
		}
	}

	if lastUsage < 0 {
		return cu
	}

	tokens := contextTokens(entries[lastUsage].Message.Usage)
	for i := lastUsage + 1; i < len(entries); i++ {
		tokens += estimateEntryTokens(entries[i])
	}
	percent := int(math.Round(float64(tokens) / float64(window) * 100))
	cu.Tokens = &tokens
	cu.Percent = &percent
	return cu
}

// validAssistantUsage reports whether an assistant message carries usage data
// that reflects a real response (not aborted/error, not all-zero).
func validAssistantUsage(m *Message) bool {
	if m.Usage == nil || m.StopReason == StopAborted || m.StopReason == StopError {
		return false
	}
	return contextTokens(m.Usage) > 0
}

// contextTokens uses the provider's totalTokens when available, else sums the
// components (pi's calculateContextTokens).
func contextTokens(u *Usage) int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.Input + u.Output + u.CacheRead + u.CacheWrite
}

// estimateEntryTokens is a chars/4 heuristic for the current context size of
// entries without usage, mirroring pi's estimateTokens.
func estimateEntryTokens(e Entry) int {
	if e.Message != nil {
		return estimateMessageTokens(e.Message)
	}
	switch e.Type {
	case TypeCompaction, TypeBranchSummary:
		return (len([]rune(e.Summary)) + 3) / 4
	}
	return 0
}

func estimateMessageTokens(m *Message) int {
	chars := 0
	switch m.Role {
	case RoleAssistant:
		for _, b := range m.Content {
			switch b.Type {
			case BlockText:
				chars += len([]rune(b.Text))
			case BlockThinking:
				chars += len([]rune(b.Thinking))
			case BlockToolCall:
				args, _ := json.Marshal(b.Arguments)
				chars += len(b.Name) + len(args)
			}
		}
	default:
		// user, toolResult, bashExecution, custom: text + images.
		for _, b := range m.Content {
			switch b.Type {
			case BlockText:
				chars += len([]rune(b.Text))
			case BlockImage:
				chars += 4800
			}
		}
	}
	return (chars + 3) / 4
}
