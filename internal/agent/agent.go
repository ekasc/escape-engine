package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/ekasc/pi-go/internal/provider"
	"github.com/ekasc/pi-go/internal/session"
	"github.com/ekasc/pi-go/internal/tools"
)

// ErrBusy is returned when Send is called while a turn is running.
var ErrBusy = errors.New("agent busy: a turn is already running")

// NamingSystemPrompt is the system prompt for the auto-naming meta-call. Fake
// providers key off this exact string, so keep it stable.
const NamingSystemPrompt = "You are naming a coding-agent conversation. Reply with ONLY a short title (3-6 words, no quotes, no period)."

// RecapSystemPrompt is the system prompt for the recap meta-call.
const RecapSystemPrompt = "You are summarizing a coding-agent conversation. Reply with ONLY a single line that begins with \"Recap: \" and then 1-3 short sentences describing the recent changes and the current state (not the full history)."

// IsNamingRequest reports whether req is the auto-naming meta-call.
func IsNamingRequest(req provider.Request) bool {
	return len(req.Messages) > 0 && req.Messages[0].Role == "system" &&
		strings.HasPrefix(req.Messages[0].Text, NamingSystemPrompt)
}

// IsRecapRequest reports whether req is the recap meta-call.
func IsRecapRequest(req provider.Request) bool {
	return len(req.Messages) > 0 && req.Messages[0].Role == "system" &&
		strings.HasPrefix(req.Messages[0].Text, RecapSystemPrompt)
}

// Options configures an Agent.
type Options struct {
	Store         *session.Store
	Provider      provider.Provider
	Tools         []tools.Tool
	Cwd           string
	Model         string
	SystemPrompt  string
	MaxIterations int // per-turn tool loop guard
	MaxRetries    int // bounded provider retries
}

func (o Options) withDefaults() Options {
	if o.MaxIterations == 0 {
		o.MaxIterations = 40
	}
	if o.MaxRetries == 0 {
		o.MaxRetries = 2
	}
	if o.SystemPrompt == "" {
		o.SystemPrompt = "You are a coding agent working in " + o.Cwd + ". " +
			"Use the available tools to inspect and change files. Be concise and precise."
	}
	return o
}

// State is the agent's lifecycle state.
type State string

const (
	StateIdle    State = "idle"
	StateRunning State = "running"
)

// TurnInfo is the payload for the `state` RPC method.
type TurnInfo struct {
	State        State    `json:"state"`
	TurnID       string   `json:"turnId,omitempty"`
	Model        string   `json:"model"`
	StartedAt    int64    `json:"startedAt,omitempty"`
	SettleReason string   `json:"settleReason,omitempty"`
	Capabilities []string `json:"capabilities"`
}

// Agent runs turns against one session file.
type Agent struct {
	opts Options
	bus  *Bus
	reg  *tools.Registry

	mu           sync.Mutex
	state        State
	turnID       string
	turnStart    time.Time
	settleReason string
	settled      chan struct{}
	cancel       context.CancelFunc

	titleSet bool
}

// New builds an agent. The store must already be open.
func New(opts Options) *Agent {
	opts = opts.withDefaults()
	return &Agent{
		opts:  opts,
		bus:   NewBus(),
		reg:   tools.New(opts.Tools...),
		state: StateIdle,
	}
}

// Events subscribes to the agent's event stream.
func (a *Agent) Events() chan Event { return a.bus.Subscribe() }

// Unsubscribe removes an event subscription.
func (a *Agent) Unsubscribe(ch chan Event) { a.bus.Unsubscribe(ch) }

// State returns a snapshot for the `state` RPC method.
func (a *Agent) State() TurnInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	info := TurnInfo{State: a.state, TurnID: a.turnID, Model: a.opts.Model, SettleReason: a.settleReason, Capabilities: []string{"recap", "auto-naming"}}
	if !a.turnStart.IsZero() {
		info.StartedAt = a.turnStart.UnixMilli()
	}
	return info
}

// Send starts a turn with the given user text. It returns immediately with
// the turn id; events (message_start/delta/end, tool_call, agent_settled)
// arrive on Events(). Returns ErrBusy if a turn is already running.
func (a *Agent) Send(text string) (string, error) {
	a.mu.Lock()
	if a.state == StateRunning {
		a.mu.Unlock()
		return "", ErrBusy
	}
	a.state = StateRunning
	a.turnID = session.NewShortID()
	a.turnStart = time.Now()
	a.settleReason = ""
	a.settled = make(chan struct{})

	// Create the turn context here so Stop/Steer always have a valid cancel,
	// even before the turn goroutine starts running.
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.mu.Unlock()

	go a.runTurn(ctx, text)
	return a.turnID, nil
}

// Stop aborts the current turn. The loop settles with reason "stopped".
func (a *Agent) Stop() {
	a.mu.Lock()
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
}

// Steer interrupts the current turn (if any), waits for it to settle, then
// starts a new turn with text. Blocks until the new turn starts.
func (a *Agent) Steer(text string) (string, error) {
	a.mu.Lock()
	ch := a.settled
	if a.state == StateRunning && a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
	if ch != nil {
		<-ch
	}
	return a.Send(text)
}

// Wait blocks until the current turn settles (no-op when idle).
func (a *Agent) Wait() {
	a.mu.Lock()
	ch := a.settled
	a.mu.Unlock()
	if ch != nil {
		<-ch
	}
}

func (a *Agent) publish(ev Event) { a.bus.Publish(ev) }

func (a *Agent) settle(reason string) {
	a.mu.Lock()
	turnID := a.turnID
	a.state = StateIdle
	a.settleReason = reason
	ch := a.settled
	a.settled = nil
	a.mu.Unlock()

	a.publish(Event{Event: EventSettled, TurnID: turnID, Reason: reason})
	if ch != nil {
		close(ch)
	}
}

// runTurn executes one full send -> model -> tools -> settle cycle.
func (a *Agent) runTurn(ctx context.Context, text string) {
	turnID := a.currentTurnID()
	a.publish(Event{Event: EventTurnStarted, TurnID: turnID, Text: text})

	defer func() {
		if r := recover(); r != nil {
			a.publish(Event{Event: EventError, Message: fmt.Sprintf("internal error: %v", r)})
			a.settle(ReasonError)
		}
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 1. Persist the user message.
	userMsg := &session.Message{
		Role:      session.RoleUser,
		Content:   []session.Block{{Type: session.BlockText, Text: text}},
		Timestamp: session.NowMillis(),
	}
	userEntry, err := a.opts.Store.Append(session.Entry{Type: session.TypeMessage, Message: userMsg})
	if err != nil {
		a.publish(Event{Event: EventError, Message: fmt.Sprintf("failed to persist user message: %v", err)})
		a.settle(ReasonError)
		return
	}
	a.publish(Event{Event: EventMessageEnd, TurnID: turnID, Role: session.RoleUser, MessageID: userEntry.ID, Timestamp: userMsg.Timestamp})

	// 2. Auto-name once per session: if no title exists yet, ask the provider
	// for one and persist it as a session_info entry (best-effort).
	if !a.titleSet {
		a.titleSet = true
		if existing, err := session.Name(a.opts.Store.Path()); err == nil && existing == "" {
			a.nameSession(ctx, text, userEntry.ID)
		}
	}

	// 3. The loop: model -> tools -> model until no tool calls remain.
	for iter := 1; ; iter++ {
		if iter > a.opts.MaxIterations {
			a.publish(Event{Event: EventError, Message: "max iterations reached"})
			a.settle(ReasonError)
			return
		}
		if ctx.Err() != nil {
			a.finishAborted(turnID, nil)
			return
		}

		hist, err := historyFromStore(a.opts.Store.Path(), a.opts.SystemPrompt)
		if err != nil {
			a.publish(Event{Event: EventError, Message: fmt.Sprintf("failed to build history: %v", err)})
			a.settle(ReasonError)
			return
		}
		req := provider.Request{Model: a.opts.Model, Messages: hist, Tools: a.reg.Specs()}

		stream, err := a.streamWithRetry(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				a.finishAborted(turnID, nil)
				return
			}
			a.publish(Event{Event: EventError, Message: fmt.Sprintf("provider error: %v", err)})
			a.settle(ReasonError)
			return
		}

		out, stopReason, err := a.consumeStream(ctx, turnID, stream, iter)
		stream.Close()
		if err != nil {
			if ctx.Err() != nil {
				a.finishAborted(turnID, out)
				return
			}
			a.finishError(turnID, out, err)
			return
		}

		assistantEntry, err := a.appendAssistant(turnID, out, stopReason)
		if err != nil {
			a.publish(Event{Event: EventError, Message: fmt.Sprintf("failed to persist assistant message: %v", err)})
			a.settle(ReasonError)
			return
		}

		if len(out.ToolCalls) == 0 {
			a.settle(ReasonDone)
			return
		}
		for _, tc := range out.ToolCalls {
			a.runOneTool(ctx, turnID, tc, assistantEntry.ID)
			if ctx.Err() != nil {
				a.finishAborted(turnID, nil)
				return
			}
		}
	}
}

type turnOutput struct {
	Text      strings.Builder
	Thinking  strings.Builder
	ToolCalls []provider.ToolCall
	Usage     provider.Usage
}

func (o *turnOutput) empty() bool {
	return o.Text.Len() == 0 && o.Thinking.Len() == 0 && len(o.ToolCalls) == 0
}

// consumeStream reads a provider stream into a turnOutput, emitting
// message_start/message_delta events as text arrives.
func (a *Agent) consumeStream(ctx context.Context, turnID string, stream provider.Stream, iter int) (*turnOutput, string, error) {
	out := &turnOutput{}
	stopReason := "stop"
	a.publish(Event{Event: EventMessageStart, TurnID: turnID, Role: session.RoleAssistant, Iteration: iter})
	for {
		ev, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, stopReason, err
		}
		switch ev.Kind {
		case provider.EventText:
			out.Text.WriteString(ev.Text)
			a.publish(Event{Event: EventMessageDelta, TurnID: turnID, Role: session.RoleAssistant, Text: ev.Text})
		case provider.EventThinking:
			out.Thinking.WriteString(ev.Thinking)
		case provider.EventToolCall:
			out.ToolCalls = append(out.ToolCalls, ev.ToolCall)
		case provider.EventDone:
			if ev.StopReason != "" {
				stopReason = ev.StopReason
			}
			out.Usage = ev.Usage
		}
	}
	if ctx.Err() != nil {
		return out, stopReason, ctx.Err()
	}
	return out, stopReason, nil
}

// streamWithRetry calls the provider, retrying retryable errors with bounded
// backoff (max MaxRetries attempts).
func (a *Agent) streamWithRetry(ctx context.Context, req provider.Request) (provider.Stream, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		s, err := a.opts.Provider.Stream(ctx, req)
		if err == nil {
			return s, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt >= a.opts.MaxRetries {
			return nil, lastErr
		}
		var re *provider.RetryableError
		if !errors.As(err, &re) {
			return nil, lastErr
		}
		delay := time.Duration(300*(1<<attempt)) * time.Millisecond
		a.publish(Event{Event: EventError, Message: fmt.Sprintf("provider error (retrying in %s): %v", delay, err)})
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// appendAssistant persists the accumulated assistant message and emits
// message_end.
func (a *Agent) appendAssistant(turnID string, out *turnOutput, stopReason string) (session.Entry, error) {
	var blocks []session.Block
	if out.Thinking.Len() > 0 {
		blocks = append(blocks, session.Block{Type: session.BlockThinking, Thinking: out.Thinking.String()})
	}
	if out.Text.Len() > 0 {
		blocks = append(blocks, session.Block{Type: session.BlockText, Text: out.Text.String()})
	}
	for _, tc := range out.ToolCalls {
		blocks = append(blocks, session.Block{Type: session.BlockToolCall, ID: tc.ID, Name: tc.Name, Arguments: tc.Args})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, session.Block{Type: session.BlockText})
	}

	sr := stopReason
	if len(out.ToolCalls) > 0 {
		sr = session.StopToolUse
	}
	msg := &session.Message{
		Role:       session.RoleAssistant,
		Content:    blocks,
		Model:      a.opts.Model,
		Provider:   "pi-go",
		StopReason: sr,
		Usage:      toSessionUsage(out.Usage),
		Timestamp:  session.NowMillis(),
	}
	entry, err := a.opts.Store.Append(session.Entry{Type: session.TypeMessage, Message: msg})
	if err != nil {
		return entry, err
	}
	a.publish(Event{Event: EventMessageEnd, TurnID: turnID, Role: session.RoleAssistant, MessageID: entry.ID, Model: a.opts.Model, Timestamp: msg.Timestamp})
	return entry, nil
}

// runOneTool executes one tool call, persists the toolResult entry and emits
// tool_call/tool_result events.
func (a *Agent) runOneTool(ctx context.Context, turnID string, tc provider.ToolCall, parentID string) {
	a.publish(Event{Event: EventToolCall, TurnID: turnID, ToolCallID: tc.ID, Name: tc.Name, Args: tc.Args})

	var res tools.Result
	if t, ok := a.reg.Get(tc.Name); ok {
		res = t.Run(ctx, tc.Args)
	} else {
		res = tools.Result{Output: fmt.Sprintf("unknown tool %q", tc.Name), IsError: true}
	}

	msg := &session.Message{
		Role:       session.RoleToolResult,
		ToolCallID: tc.ID,
		ToolName:   tc.Name,
		Content:    []session.Block{{Type: session.BlockText, Text: res.Output}},
		IsError:    res.IsError,
		Timestamp:  session.NowMillis(),
	}
	_, err := a.opts.Store.Append(session.Entry{Type: session.TypeMessage, ParentID: parentID, Message: msg})
	if err != nil {
		a.publish(Event{Event: EventError, Message: fmt.Sprintf("failed to persist tool result: %v", err)})
	}
	a.publish(Event{Event: EventToolResult, TurnID: turnID, ToolCallID: tc.ID, Output: res.Output, Error: res.IsError})
}

// finishAborted persists partial output (if any) and settles "stopped".
func (a *Agent) finishAborted(turnID string, out *turnOutput) {
	if out != nil && !out.empty() {
		_, _ = a.appendAssistant(turnID, out, session.StopAborted)
	}
	a.settle(ReasonStopped)
}

// finishError persists partial output (if any), emits the error and settles
// "error".
func (a *Agent) finishError(turnID string, out *turnOutput, err error) {
	if out != nil && !out.empty() {
		_, _ = a.appendAssistant(turnID, out, session.StopError)
	}
	a.publish(Event{Event: EventError, Message: err.Error()})
	a.settle(ReasonError)
}

func (a *Agent) currentTurnID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turnID
}

// callText makes a bounded meta-call (no tools, no retries, no events) and
// returns the concatenated text. Used for naming and recap.
func (a *Agent) callText(ctx context.Context, messages []provider.Message, maxTokens int) (string, error) {
	req := provider.Request{Model: a.opts.Model, Messages: messages, MaxTokens: maxTokens}
	stream, err := a.opts.Provider.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	var b strings.Builder
	for {
		ev, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return b.String(), err
		}
		if ev.Kind == provider.EventText {
			b.WriteString(ev.Text)
		}
	}
	return b.String(), nil
}

// nameSession asks the provider for a short title for the first user message
// and appends it as a session_info entry. Best-effort: any failure returns
// silently and leaves the session untitled.
func (a *Agent) nameSession(ctx context.Context, text, parentID string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := a.callText(ctx, []provider.Message{
		{Role: "system", Text: NamingSystemPrompt},
		{Role: "user", Text: text},
	}, 200)
	if err != nil {
		return
	}
	name := cleanTitle(out)
	if name == "" {
		return
	}
	_, _ = a.opts.Store.Append(session.Entry{Type: session.TypeSessionInfo, Name: name, ParentID: parentID})
}

// Recap summarizes the recent conversation into a single "Recap: ..." line,
// persists it as a session_info entry (carrying the existing title forward),
// and returns the line. Safe to call while a turn is running.
func (a *Agent) Recap(ctx context.Context) (string, error) {
	entries, err := session.ReadAll(a.opts.Store.Path())
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := a.callText(ctx, []provider.Message{
		{Role: "system", Text: RecapSystemPrompt},
		{Role: "user", Text: recentMessages(entries, 20)},
	}, 300)
	if err != nil {
		return "", err
	}
	line := cleanRecap(out)
	if line == "" {
		return "", errors.New("provider returned an empty recap")
	}

	title := session.LastName(entries)
	entry := session.Entry{Type: session.TypeSessionInfo, Recap: line}
	if title != "" {
		entry.Name = title
	}
	if _, err := a.opts.Store.Append(entry); err != nil {
		return "", err
	}
	return line, nil
}

// recentMessages renders the message stretch to summarize: the last max
// message entries, or everything after the last recap marker (a session_info
// entry carrying a recap field) if one exists.
func recentMessages(entries []session.Entry, max int) string {
	lastRecap := -1
	for i, e := range entries {
		if e.Type == session.TypeSessionInfo && strings.TrimSpace(e.Recap) != "" {
			lastRecap = i
		}
	}

	var msgs []session.Entry
	for i := lastRecap + 1; i < len(entries); i++ {
		if entries[i].Type == session.TypeMessage && entries[i].Message != nil {
			msgs = append(msgs, entries[i])
		}
	}
	if len(msgs) > max {
		msgs = msgs[len(msgs)-max:]
	}

	var b strings.Builder
	for _, m := range msgs {
		text := strings.TrimSpace(m.Message.Text(false))
		if text == "" {
			continue
		}
		role := m.Message.Role
		if role == session.RoleToolResult {
			role = "tool"
		}
		b.WriteString(role + ": " + truncateText(text, 400) + "\n")
	}
	return b.String()
}

// cleanTitle normalizes a provider-returned title: strips quotes, a trailing
// period, and collapses whitespace.
func cleanTitle(s string) string {
	t := strings.TrimSpace(s)
	t = strings.Trim(t, "\"'`\u201c\u201d\u2018\u2019")
	t = strings.TrimSuffix(strings.TrimSpace(t), ".")
	t = strings.Join(strings.Fields(t), " ")
	r := []rune(t)
	if len(r) > 60 {
		t = string(r[:59]) + "…"
	}
	return t
}

// cleanRecap collapses a provider recap to a single "Recap: ..." line.
func cleanRecap(s string) string {
	t := strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if t == "" {
		return ""
	}
	if strings.HasPrefix(t, "Recap: ") {
		return t
	}
	if strings.HasPrefix(t, "Recap:") {
		return "Recap: " + strings.TrimSpace(strings.TrimPrefix(t, "Recap:"))
	}
	return "Recap: " + t
}

func truncateText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func toSessionUsage(u provider.Usage) *session.Usage {
	return &session.Usage{
		Input:       u.Input,
		Output:      u.Output,
		CacheRead:   u.CacheRead,
		CacheWrite:  u.CacheWrite,
		TotalTokens: u.TotalTokens,
	}
}
