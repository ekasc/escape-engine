// Package session implements the pi-compatible JSONL session store.
//
// The on-disk shape is the contract the desktop shell's reader
// (electron/sessions.ts -> readSessionTail/readSessionRange/readSessionInfo)
// depends on, so these files must stay readable by it unchanged:
//
//   - one JSON object per line, append-only
//   - a `session` header entry: {type:"session", id, cwd, timestamp}
//   - `message` entries with `entry.message` carrying `role`, `content`
//     (array of {type:"text",text} blocks or a plain string) and a numeric
//     `timestamp`
//   - `session_info` entries with a `name` field (used as the chat title)
//   - tool results as role "toolResult" with `toolCallId` on the message
package session

import (
	"encoding/json"
	"time"
)

// Entry is one JSONL line in a session file. Field names and semantics follow
// pi's session format v3.
//
// Compaction and branch_summary entries carry their data as dedicated flat
// fields (summary, firstKeptEntryId, tokensBefore, fromId, details, usage),
// matching pi's on-disk shape byte for byte.
type Entry struct {
	Type          string   `json:"type"`
	Version       int      `json:"version,omitempty"`
	ID            string   `json:"id"`
	ParentID      string   `json:"parentId,omitempty"`
	Timestamp     string   `json:"timestamp"`
	Cwd           string   `json:"cwd,omitempty"`
	ParentSession string   `json:"parentSession,omitempty"`
	Name          string   `json:"name,omitempty"`
	Recap         string   `json:"recap,omitempty"`
	Provider      string   `json:"provider,omitempty"`
	ModelID       string   `json:"modelId,omitempty"`
	Model         string   `json:"model,omitempty"`
	Message       *Message `json:"message,omitempty"`

	// Compaction entry fields (type "compaction"): summary of the compacted
	// prefix, id of the first entry kept after the cut, tokens consumed before
	// compaction, optional file-tracking details and summary-generation usage.
	Summary          string `json:"summary,omitempty"`
	FirstKeptEntryID string `json:"firstKeptEntryId,omitempty"`
	TokensBefore     int    `json:"tokensBefore,omitempty"`
	SnapcompactData  string `json:"snapcompactData,omitempty"`
	FromHook         bool   `json:"fromHook,omitempty"`

	// BranchSummary entry fields (type "branch_summary"): summary of the
	// abandoned branch and the entry id the branch departed from.
	FromID string `json:"fromId,omitempty"`

	// Details is implementation-specific data shared by compaction and
	// branch_summary entries (e.g. {readFiles, modifiedFiles}).
	Details any `json:"details,omitempty"`
	// Usage is the LLM usage from generating a compaction/branch_summary
	// summary, included in session token and cost totals.
	Usage *Usage `json:"usage,omitempty"`
}

// Entry types written by this runtime.
const (
	TypeSession       = "session"
	TypeMessage       = "message"
	TypeSessionInfo   = "session_info"
	TypeModelChange   = "model_change"
	TypeCompaction    = "compaction"
	TypeBranchSummary = "branch_summary"
	TypeCustom        = "custom_message"
)

// Message roles (pi-compatible).
const (
	RoleUser          = "user"
	RoleAssistant     = "assistant"
	RoleToolResult    = "toolResult"
	RoleBashExecution = "bashExecution"
)

// Stop reasons as persisted by pi.
const (
	StopStop    = "stop"
	StopLength  = "length"
	StopToolUse = "toolUse"
	StopError   = "error"
	StopAborted = "aborted"
)

// Message is the `message` field of a message entry.
type Message struct {
	Role         string  `json:"role"`
	Content      []Block `json:"content"`
	ToolCallID   string  `json:"toolCallId,omitempty"`
	ToolName     string  `json:"toolName,omitempty"`
	IsError      bool    `json:"isError,omitempty"`
	Model        string  `json:"model,omitempty"`
	Provider     string  `json:"provider,omitempty"`
	StopReason   string  `json:"stopReason,omitempty"`
	Usage        *Usage  `json:"usage,omitempty"`
	ErrorMessage string  `json:"errorMessage,omitempty"`
	Timestamp    int64   `json:"timestamp"`
}

// Block is one content block of a message.
type Block struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	Thinking  string         `json:"thinking,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Data      string         `json:"data,omitempty"`
	MimeType  string         `json:"mimeType,omitempty"`
	Source    string         `json:"source,omitempty"`
}

// Block types.
const (
	BlockText     = "text"
	BlockThinking = "thinking"
	BlockToolCall = "toolCall"
	// BlockImage is an image attachment. BlockMedia is the same shape for
	// everything else a user can drop on the composer. Both exist so that files
	// written before non-image attachments do not have to be rewritten: a
	// session file that says "image" keeps saying it, and readers treat the two
	// as the same thing. They are not aliases on disk — the value is the value.
	BlockImage = "image"
	BlockMedia = "media"
)

// Usage mirrors pi's token accounting (costs omitted in v1).
type Usage struct {
	Input       int `json:"input"`
	Output      int `json:"output"`
	CacheRead   int `json:"cacheRead,omitempty"`
	CacheWrite  int `json:"cacheWrite,omitempty"`
	TotalTokens int `json:"totalTokens"`
}

// Text extracts the concatenated text of all text blocks (and thinking, when
// includeThinking is set). Used for history building and summaries.
func (m *Message) Text(includeThinking bool) string {
	var out []byte
	for _, b := range m.Content {
		switch b.Type {
		case BlockText:
			out = append(out, b.Text...)
		case BlockThinking:
			if includeThinking {
				out = append(out, b.Thinking...)
			}
		}
	}
	return string(out)
}

// ToolCalls returns the toolCall blocks in order.
func (m *Message) ToolCalls() []Block {
	var calls []Block
	for _, b := range m.Content {
		if b.Type == BlockToolCall {
			calls = append(calls, b)
		}
	}
	return calls
}

// NowISO returns the entry-level timestamp format pi uses (RFC3339, ms, UTC).
func NowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// NowMillis returns the numeric timestamp used inside messages (Unix ms).
func NowMillis() int64 {
	return time.Now().UnixMilli()
}

// Marshal writes the entry as a single JSONL line (without trailing newline).
func (e Entry) Marshal() ([]byte, error) {
	return json.Marshal(e)
}
