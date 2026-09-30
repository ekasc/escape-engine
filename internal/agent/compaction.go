package agent

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/ekasc/escape-engine/internal/provider"
	"github.com/ekasc/escape-engine/internal/session"
)

// CompactionResult is the outcome of a compaction pass.
type CompactionResult struct {
	Summary              string         `json:"summary"`
	FirstKeptEntryID     string         `json:"firstKeptEntryId"`
	TokensBefore         int            `json:"tokensBefore"`
	EstimatedTokensAfter int            `json:"estimatedTokensAfter"`
	Usage                provider.Usage `json:"usage,omitempty"`
}

// CompactionDetails tracks file operations cumulatively across compactions.
type CompactionDetails struct {
	ReadFiles     []string `json:"readFiles"`
	ModifiedFiles []string `json:"modifiedFiles"`
}

// modelContextWindows is a small built-in map of context windows in tokens.
var modelContextWindows = map[string]int{
	"deepseek-v4-flash": 1000000,
	"deepseek-v4-pro":   1000000,
	"gpt-5.6-luna":      272000,
	"gpt-5.6-sol":       272000,
	"gpt-5.6-terra":     272000,
	"minimax-m3":        1000000,
	"glm-5.1":           1000000,
	"glm-5.2":           1000000,
	"glm-5.3":           1000000,
}

func contextWindowFor(model string) int {
	if w, ok := modelContextWindows[model]; ok {
		return w
	}
	// An unknown model is a real problem rather than a number to guess. 200,000
	// is optimistic: it made compaction believe it had headroom it did not
	// have, so a request went out that the provider rejected outright. A window
	// that errs low compacts early, which costs a summary; a window that errs
	// high fails the turn.
	return DefaultContextWindow
}

// DefaultContextWindow is used for any model not in the table. It is
// deliberately smaller than the windows of the models that serve most sessions,
// so an unknown model compacts before it breaks rather than after.
const DefaultContextWindow = 128000

// ContextReserve is held back from the window for the response. A model cannot
// spend the whole window on input and still have room to answer.
const ContextReserve = 20000

// usableContext is how much of the window may be filled with input.
func usableContext(model string) int {
	w := contextWindowFor(model)
	if w <= ContextReserve {
		return w / 2
	}
	return w - ContextReserve
}

const compactionSystemPrompt = `You are summarizing a coding-agent session to free context. Produce a structured summary in this exact Markdown format, including only sections that have content:

## Goal
[what the user is trying to accomplish]

## Constraints & Preferences
- [requirements the user mentioned]

## Progress
### Done
- [x] [completed tasks]

### In Progress
- [ ] [current work]

### Blocked
- [issues, if any]

## Key Decisions
- **[Decision]**: [rationale]

## Next Steps
1. [what should happen next]

## Critical Context
- [data needed to continue]

<read-files>
path/to/read/file
</read-files>

<modified-files>
path/to/modified/file
</modified-files>
`

// Compact performs a manual compaction with optional custom instructions,
// appends a compaction entry, and returns the result.
func (a *Agent) Compact(ctx context.Context, instructions string) (*CompactionResult, error) {
	return a.compact(ctx, "manual", instructions, "")
}

func (a *Agent) Snapcompact(_ context.Context) (*CompactionResult, error) {
	entries, err := a.history.sinceAppends(a.opts.Store.Path())
	if err != nil {
		return nil, err
	}
	keep := a.opts.Settings.Compaction.KeepRecentTokens
	if keep <= 0 {
		keep = 20000
	}
	cut := findCutPoint(entries, keep)
	if cut == 0 {
		return &CompactionResult{}, nil
	}
	tokensBefore := estimateConversationTokens(entries)
	archiveText := strings.TrimSpace(serializeConversation(entries[:cut]))
	if len(archiveText) > 200000 {
		archiveText = archiveText[len(archiveText)-200000:]
	}
	archive, err := buildSnapArchive(archiveText)
	if err != nil {
		return nil, err
	}
	summary := "Snapcompact archive created; continue from the recent conversation tail."
	a.publish(Event{Event: EventCompactionStart, Reason: "snapcompact"})
	entry, err := a.opts.Store.Append(session.Entry{
		Type:             session.TypeCompaction,
		Summary:          summary,
		FirstKeptEntryID: entries[cut].ID,
		TokensBefore:     tokensBefore,
		SnapcompactData:  archive,
		FromHook:         true,
		Details: map[string]any{
			"snapcompactGeneration": session.NewSessionID(),
			"snapcompactProfile":    "go-text-v1",
			"readFiles":             collectReadFiles(entries[:cut]),
			"modifiedFiles":         collectModifiedFiles(entries[:cut]),
		},
	})
	if err != nil {
		return nil, err
	}
	result := &CompactionResult{Summary: summary, FirstKeptEntryID: entries[cut].ID, TokensBefore: tokensBefore, EstimatedTokensAfter: estimateConversationTokens(append(entries[cut:], entry))}
	a.publish(Event{Event: EventCompactionEnd, Reason: "snapcompact", Summary: summary, FirstKeptEntryID: result.FirstKeptEntryID, TokensBefore: tokensBefore, EstimatedTokensAfter: result.EstimatedTokensAfter})
	return result, nil
}

// autoCompactIfNeeded compacts when the conversation is near the model's
// context window. Best-effort: callers ignore the returned error.
// autoCompactIfNeeded compacts when the conversation is near the model's
// context window. It reports whether it actually compacted, so a turn can
// record that it paid for one.
func (a *Agent) autoCompactIfNeeded(ctx context.Context) (bool, error) {
	if !a.opts.Settings.Compaction.Enabled {
		return false, nil
	}
	entries, err := a.history.sinceAppends(a.opts.Store.Path())
	if err != nil {
		return false, err
	}
	// The budget is the model's usable input window, and what is measured
	// against it is everything the request carries, not just the conversation.
	reserve := a.opts.Settings.Compaction.ReserveTokens
	if reserve <= 0 {
		reserve = ContextReserve
	}
	usable := usableContext(a.opts.Model)
	if r := contextWindowFor(a.opts.Model) - reserve; r < usable {
		usable = r
	}
	if a.estimateRequestTokens(entries) <= usable {
		return false, nil
	}
	if _, err = a.compact(ctx, "threshold", "", ""); err != nil {
		return false, err
	}
	return true, nil
}

func (a *Agent) compact(ctx context.Context, reason, instructions, turnID string) (*CompactionResult, error) {
	entries, err := a.history.sinceAppends(a.opts.Store.Path())
	if err != nil {
		return nil, err
	}

	keep := a.opts.Settings.Compaction.KeepRecentTokens
	if keep <= 0 {
		keep = 20000
	}
	cut := findCutPoint(entries, keep)
	tokensBefore := estimateConversationTokens(entries)

	a.publish(Event{Event: EventCompactionStart, TurnID: turnID, Reason: reason})

	previous := lastCompactionSummary(entries[:cut])
	summary := a.summarize(ctx, entries[:cut], previous, instructions)

	firstKept := ""
	if cut < len(entries) {
		firstKept = entries[cut].ID
	}

	details := CompactionDetails{
		ReadFiles:     union(collectReadFiles(entries[:cut]), priorReadFiles(entries[:cut])),
		ModifiedFiles: union(collectModifiedFiles(entries[:cut]), priorModifiedFiles(entries[:cut])),
	}

	entry, err := a.opts.Store.Append(session.Entry{
		Type:             session.TypeCompaction,
		Summary:          summary,
		FirstKeptEntryID: firstKept,
		TokensBefore:     tokensBefore,
		Details:          details,
	})
	if err != nil {
		return nil, err
	}

	result := &CompactionResult{
		Summary:              summary,
		FirstKeptEntryID:     firstKept,
		TokensBefore:         tokensBefore,
		EstimatedTokensAfter: estimateConversationTokens(append(entries[cut:], entry)),
	}
	a.publish(Event{
		Event:                EventCompactionEnd,
		TurnID:               turnID,
		Reason:               reason,
		Summary:              summary,
		FirstKeptEntryID:     firstKept,
		TokensBefore:         tokensBefore,
		EstimatedTokensAfter: result.EstimatedTokensAfter,
	})
	return result, nil
}

// SummarizeBranch appends a branch_summary entry summarizing the entries on
// the branch being abandoned (from the common ancestor with targetID up to
// fromID).
func (a *Agent) SummarizeBranch(ctx context.Context, fromID, targetID string) error {
	entries, err := a.history.sinceAppends(a.opts.Store.Path())
	if err != nil {
		return err
	}
	ancestor, err := session.CommonAncestor(a.opts.Store.Path(), fromID, targetID)
	if err != nil {
		return err
	}
	toSummarize := branchEntriesUpTo(entries, fromID, ancestor)
	if len(toSummarize) == 0 {
		return nil
	}
	summary := a.summarize(ctx, toSummarize, "", "")
	_, err = a.opts.Store.Append(session.Entry{
		Type:    session.TypeBranchSummary,
		Summary: summary,
		FromID:  fromID,
		Details: CompactionDetails{
			ReadFiles:     collectReadFiles(toSummarize),
			ModifiedFiles: collectModifiedFiles(toSummarize),
		},
	})
	return err
}

// summarize calls the provider for a structured summary of the entries.
func (a *Agent) summarize(ctx context.Context, entries []session.Entry, previous, instructions string) string {
	text := serializeConversation(entries)
	var b strings.Builder
	if previous != "" {
		b.WriteString("Previous summary:\n" + previous + "\n\n")
	}
	b.WriteString("Conversation:\n" + text)
	if instructions != "" {
		b.WriteString("\n\nAdditional instructions: " + instructions)
	}
	out, err := a.callText(ctx, "", []provider.Message{
		{Role: "system", Text: compactionSystemPrompt},
		{Role: "user", Text: b.String()},
	}, 3000)
	if err != nil || strings.TrimSpace(out) == "" {
		return fallbackSummary(text)
	}
	return strings.TrimSpace(out)
}

// fallbackSummary is a minimal summary used when the provider call fails.
func fallbackSummary(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > 2000 {
		text = text[:2000]
	}
	return "## Critical Context\n" + text
}

// findCutPoint returns the index of the first entry to keep. It walks back
// from the newest entry accumulating token estimates until keepTokens is
// reached, then retreats to the start of that turn (the user message), so
// the cut always lands on a turn boundary and never at a tool result.
func findCutPoint(entries []session.Entry, keepTokens int) int {
	acc := 0
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if !isContentEntry(e) {
			continue
		}
		acc += estimateEntryTokens(e)
		if acc >= keepTokens {
			for i > 0 && !isUserMessage(entries[i]) {
				i--
			}
			if i < 0 {
				i = 0
			}
			return i
		}
	}
	return 0
}

// serializeConversation renders entries as role-tagged text so the model
// treats them as a transcript to summarize, not a conversation to continue.
func serializeConversation(entries []session.Entry) string {
	var b strings.Builder
	for _, e := range entries {
		if e.Type != session.TypeMessage || e.Message == nil {
			continue
		}
		m := e.Message
		switch m.Role {
		case session.RoleUser:
			b.WriteString("[User]: " + m.Text(false) + "\n")
		case session.RoleAssistant:
			for _, blk := range m.Content {
				switch blk.Type {
				case session.BlockThinking:
					b.WriteString("[Assistant thinking]: " + truncateN(blk.Thinking, 2000) + "\n")
				case session.BlockText:
					b.WriteString("[Assistant]: " + blk.Text + "\n")
				}
			}
			if calls := m.ToolCalls(); len(calls) > 0 {
				var parts []string
				for _, c := range calls {
					parts = append(parts, c.Name+"("+formatArgs(c.Arguments)+")")
				}
				b.WriteString("[Assistant tool calls]: " + strings.Join(parts, "; ") + "\n")
			}
		case session.RoleToolResult:
			b.WriteString("[Tool result]: " + truncateN(m.Text(false), 2000) + "\n")
		}
	}
	return b.String()
}

func formatArgs(args map[string]any) string {
	b, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func truncateN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// collectReadFiles extracts files read via the `read` tool.
func collectReadFiles(entries []session.Entry) []string {
	return collectFiles(entries, "read")
}

// collectModifiedFiles extracts files changed via `write`/`edit` tools.
func collectModifiedFiles(entries []session.Entry) []string {
	return union(collectFiles(entries, "write"), collectFiles(entries, "edit"))
}

func collectFiles(entries []session.Entry, tool string) []string {
	var files []string
	for _, e := range entries {
		if e.Type != session.TypeMessage || e.Message == nil || e.Message.Role != session.RoleAssistant {
			continue
		}
		for _, b := range e.Message.Content {
			if b.Type != session.BlockToolCall || b.Name != tool {
				continue
			}
			if p, ok := b.Arguments["path"].(string); ok && p != "" {
				files = append(files, p)
			}
		}
	}
	return files
}

func priorReadFiles(entries []session.Entry) []string     { return priorFiles(entries, true) }
func priorModifiedFiles(entries []session.Entry) []string { return priorFiles(entries, false) }

// priorFiles reads file operations carried forward in prior compaction or
// branch_summary details.
func priorFiles(entries []session.Entry, read bool) []string {
	var files []string
	for _, e := range entries {
		if e.Type != session.TypeCompaction && e.Type != session.TypeBranchSummary {
			continue
		}
		if e.Details == nil {
			continue
		}
		b, err := json.Marshal(e.Details)
		if err != nil {
			continue
		}
		var d CompactionDetails
		if err := json.Unmarshal(b, &d); err != nil {
			continue
		}
		if read {
			files = append(files, d.ReadFiles...)
		} else {
			files = append(files, d.ModifiedFiles...)
		}
	}
	return files
}

func union(sets ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, set := range sets {
		for _, s := range set {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

func lastCompactionSummary(entries []session.Entry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type == session.TypeCompaction && strings.TrimSpace(entries[i].Summary) != "" {
			return entries[i].Summary
		}
	}
	return ""
}

// branchEntriesUpTo collects entries on the path from leafID up to (but not
// including) ancestorID, in chronological order.
func branchEntriesUpTo(entries []session.Entry, leafID, ancestorID string) []session.Entry {
	byID := map[string]session.Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	var out []session.Entry
	cur := leafID
	for cur != "" && cur != ancestorID {
		e, ok := byID[cur]
		if !ok {
			break
		}
		out = append(out, e)
		cur = e.ParentID
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func isContentEntry(e session.Entry) bool {
	return e.Type == session.TypeMessage && e.Message != nil
}

func isUserMessage(e session.Entry) bool {
	return isContentEntry(e) && e.Message.Role == session.RoleUser
}

// staticOverhead is the part of every request that is not conversation: the
// system prompt and the tool schemas.
//
// It is not small. The system prompt alone is roughly 15,800 tokens because the
// skill descriptions are in it, and the tool schemas are re-sent every turn. An
// estimate that counted only the conversation would think it had tens of
// thousands of tokens free that were already spoken for, and compaction would
// fire far too late -- which is exactly how a request ends up being rejected by
// the provider instead of being summarized.
func (a *Agent) staticOverheadTokens() int {
	total := len(a.systemPrompt) / 4
	for _, spec := range a.specs {
		total += estimateSpecTokens(spec)
	}
	return total
}

// estimateSpecTokens charges a fixed per-tool cost plus its schema, since every
// request carries every tool definition whether or not the model uses it.
func estimateSpecTokens(spec provider.ToolSpec) int {
	const perTool = 80 // name, description and envelope, roughly
	raw, err := json.Marshal(spec)
	if err != nil {
		return perTool
	}
	return perTool + len(raw)/4
}

// estimateRequestTokens is everything a request would carry: the overhead plus
// the live conversation.
func (a *Agent) estimateRequestTokens(entries []session.Entry) int {
	return a.staticOverheadTokens() + estimateActiveConversationTokens(entries)
}

func estimateActiveConversationTokens(entries []session.Entry) int {
	start := 0
	for i := range entries {
		e := entries[i]
		if e.Type != session.TypeCompaction || e.FirstKeptEntryID == "" {
			continue
		}
		for j := range entries {
			if entries[j].ID == e.FirstKeptEntryID {
				start = j
				break
			}
		}
	}
	return estimateConversationTokens(entries[start:])
}

func estimateConversationTokens(entries []session.Entry) int {
	total := 0
	for _, e := range entries {
		total += estimateEntryTokens(e)
	}
	return total
}

func estimateEntryTokens(e session.Entry) int {
	if !isContentEntry(e) {
		return 0
	}
	total := 0
	for _, b := range e.Message.Content {
		switch b.Type {
		case session.BlockText:
			total += len(b.Text)
		case session.BlockThinking:
			total += len(b.Thinking)
		case session.BlockToolCall:
			args, _ := json.Marshal(b.Arguments)
			total += len(args)
		}
	}
	return total / 4
}
