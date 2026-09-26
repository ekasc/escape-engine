package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ekasc/escape-engine/internal/diagnostics"
	"github.com/ekasc/escape-engine/internal/provider"
	"github.com/ekasc/escape-engine/internal/session"
	"github.com/ekasc/escape-engine/internal/settings"
	"github.com/ekasc/escape-engine/internal/tools"
)

// ErrBusy is returned when Send is called while a turn is running.
var ErrBusy = errors.New("agent busy: a turn is already running")

// ApprovalMode controls whether side-effecting tools require user approval.
type ApprovalMode string

const (
	ApprovalAuto ApprovalMode = "auto"
	ApprovalAsk  ApprovalMode = "ask"
)

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
	Store    *session.Store
	Provider provider.Provider
	// SpillDir receives full tool output that was too large to keep inline.
	SpillDir              string
	Tools                 []tools.Tool
	Cwd                   string
	Model                 string
	SystemPrompt          string
	MaxIterations         int // per-turn tool loop guard
	MaxRetries            int // bounded provider retries
	MaxTokens             int // optional provider output-token limit
	Settings              *settings.Settings
	ApprovalMode          ApprovalMode
	QuietResourceWarnings bool
	// Memory is the project's durable notes, already rendered. It is a plain
	// string rather than a store so the agent cannot re-read it mid-session:
	// the snapshot stays frozen, which keeps the provider prefix cache valid.
	Memory string
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
	if o.Settings == nil {
		o.Settings = settings.Defaults()
	}
	switch o.ApprovalMode {
	case "":
		o.ApprovalMode = ApprovalAuto
	case ApprovalAuto, ApprovalAsk:
	default:
		// Fail closed if a caller constructs Options directly with an invalid mode.
		o.ApprovalMode = ApprovalAsk
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
	State        State        `json:"state"`
	TurnID       string       `json:"turnId,omitempty"`
	Model        string       `json:"model"`
	ApprovalMode ApprovalMode `json:"approvalMode"`
	StartedAt    int64        `json:"startedAt,omitempty"`
	SettleReason string       `json:"settleReason,omitempty"`
	Capabilities []string     `json:"capabilities"`
}

// Agent runs turns against one session file.
type Agent struct {
	opts  Options
	bus   *Bus
	reg   *tools.Registry
	specs []provider.ToolSpec

	mu           sync.Mutex
	state        State
	turnID       string
	turnStart    time.Time
	settleReason string
	settled      chan struct{}
	cancel       context.CancelFunc

	titleSet bool

	systemPrompt string
	// namingDone is closed when the concurrent title write finishes, or is nil
	// when no title is being written. Recap copies the current title into the
	// recap entry, so reading it before naming lands would persist an empty
	// one and lose the session's name.
	namingDone chan struct{}

	// diag records what each turn cost, phase by phase, so a slow turn can be
	// attributed instead of guessed at.
	diag *diagnostics.Recorder

	// history caches the parsed session entries so each turn reads only the
	// bytes appended since the last one.
	history       entryCache
	steerQueue    []string
	followUpQueue []string
	questions     map[string]chan string
	approvals     map[string]chan bool
	retryAbort    chan struct{}
}

// New builds an agent. The store must already be open.
func New(opts Options) *Agent {
	opts = opts.withDefaults()
	if opts.SpillDir == "" {
		opts.SpillDir = filepath.Join(settings.GlobalDir(), "tool-output")
	}
	reg := tools.New(opts.Tools...)
	a := &Agent{
		opts:         opts,
		bus:          NewBus(),
		reg:          reg,
		specs:        reg.Specs(),
		state:        StateIdle,
		systemPrompt: buildSystemPrompt(opts),
		questions:    make(map[string]chan string),
		approvals:    make(map[string]chan bool),
		retryAbort:   make(chan struct{}),
	}
	// Bind the session ID to the provider so transports that require it (e.g.
	// the x-opencode-session header) send it on every request, including after
	// a session switch. Previously only the CLI entry points did this, which
	// left the RPC path without a session ID.
	if setter, ok := opts.Provider.(interface{ SetSessionID(string) }); ok && opts.Store != nil {
		setter.SetSessionID(opts.Store.ID())
	}
	a.diag = diagnostics.NewRecorder()
	return a
}

// Events subscribes to the agent's event stream.
func (a *Agent) Events() chan Event { return a.bus.Subscribe() }

// Unsubscribe removes an event subscription.
func (a *Agent) Unsubscribe(ch chan Event) { a.bus.Unsubscribe(ch) }

// State returns a snapshot for the `state` RPC method.
func (a *Agent) State() TurnInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	info := TurnInfo{State: a.state, TurnID: a.turnID, Model: a.opts.Model, ApprovalMode: a.opts.ApprovalMode, SettleReason: a.settleReason, Capabilities: []string{"recap", "auto-naming"}}
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

// Diagnostics returns the turn recorder, which the RPC layer reads.
func (a *Agent) Diagnostics() *diagnostics.Recorder { return a.diag }

// SetModel changes the model used for subsequent turns.
func (a *Agent) MaxTokens() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.opts.MaxTokens
}

func (a *Agent) SetMaxTokens(maxTokens int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opts.MaxTokens = maxTokens
}

func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	a.opts.Model = model
	a.mu.Unlock()
}

func (a *Agent) SetProvider(p provider.Provider) {
	a.mu.Lock()
	a.opts.Provider = p
	if setter, ok := p.(interface{ SetSessionID(string) }); ok && a.opts.Store != nil {
		setter.SetSessionID(a.opts.Store.ID())
	}
	a.mu.Unlock()
}

// SwitchStore changes the active session after the current turn has settled.
func (a *Agent) SwitchStore(store *session.Store) error {
	if store == nil {
		return errors.New("cannot switch to a nil session store")
	}
	a.mu.Lock()
	if a.state == StateRunning {
		a.mu.Unlock()
		return errors.New("cannot switch sessions while the agent is running")
	}
	a.opts.Store = store
	a.titleSet = false
	provider := a.opts.Provider
	a.mu.Unlock()
	if setter, ok := provider.(interface{ SetSessionID(string) }); ok {
		setter.SetSessionID(store.ID())
	}
	// Parse the transcript in the background so the first message does not pay
	// for it. The turn still reads it, just after the read has already happened.
	go a.history.prime(store.Path())
	return nil
}

// Rename persists a title for the active session.
func (a *Agent) Rename(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("session name cannot be empty")
	}
	a.mu.Lock()
	if a.state == StateRunning {
		a.mu.Unlock()
		return errors.New("cannot rename session while the agent is running")
	}
	store := a.opts.Store
	a.mu.Unlock()
	if store == nil {
		return errors.New("cannot rename without an active session store")
	}
	if err := session.SetName(store.Path(), name); err != nil {
		return err
	}
	a.mu.Lock()
	a.titleSet = true
	a.mu.Unlock()
	return nil
}

// SetApprovalMode changes the policy used for subsequent side-effecting tools.
func (a *Agent) SetApprovalMode(mode ApprovalMode) error {
	if mode != ApprovalAuto && mode != ApprovalAsk {
		return fmt.Errorf("unknown approval mode %q (want auto or ask)", mode)
	}
	a.mu.Lock()
	if a.state == StateRunning {
		a.mu.Unlock()
		return errors.New("cannot change approval mode while the agent is running")
	}
	a.opts.ApprovalMode = mode
	a.mu.Unlock()
	return nil
}

// SetThinkingLevel updates the provider's reasoning effort when supported.
func (a *Agent) SetThinkingLevel(level string) {
	if ts, ok := a.opts.Provider.(interface{ SetReasoningEffort(string) }); ok {
		ts.SetReasoningEffort(level)
	}
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

// QueueSteer queues a steering message for delivery after the current
// assistant turn completes its tool calls (pi steering semantics). When the
// agent is idle it sends immediately.
func (a *Agent) QueueSteer(text string) (string, error) {
	return a.queueMessage(text, true)
}

// FollowUp queues a follow-up message for delivery after the agent settles.
// When the agent is idle it sends immediately.
func (a *Agent) FollowUp(text string) (string, error) {
	return a.queueMessage(text, false)
}

func (a *Agent) queueMessage(text string, steer bool) (string, error) {
	a.mu.Lock()
	if a.state == StateIdle {
		a.mu.Unlock()
		return a.Send(text)
	}
	if steer {
		a.steerQueue = append(a.steerQueue, text)
	} else {
		a.followUpQueue = append(a.followUpQueue, text)
	}
	steering := append([]string(nil), a.steerQueue...)
	followUp := append([]string(nil), a.followUpQueue...)
	a.mu.Unlock()
	a.publish(Event{Event: EventQueueUpdate, Steering: steering, FollowUp: followUp})
	return "", nil
}

// deliverQueued starts the next queued turn (steer priority over follow-up),
// honoring the steering/follow-up delivery modes.
func (a *Agent) deliverQueued() {
	a.mu.Lock()
	steerMode := a.opts.Settings.SteeringMode
	followMode := a.opts.Settings.FollowUpMode
	var text string
	if len(a.steerQueue) > 0 {
		if steerMode == "all" {
			text = strings.Join(a.steerQueue, "\n")
			a.steerQueue = nil
		} else {
			text = a.steerQueue[0]
			a.steerQueue = a.steerQueue[1:]
		}
	} else if len(a.followUpQueue) > 0 {
		if followMode == "all" {
			text = strings.Join(a.followUpQueue, "\n")
			a.followUpQueue = nil
		} else {
			text = a.followUpQueue[0]
			a.followUpQueue = a.followUpQueue[1:]
		}
	} else {
		a.mu.Unlock()
		return
	}
	steering := append([]string(nil), a.steerQueue...)
	followUp := append([]string(nil), a.followUpQueue...)
	a.mu.Unlock()
	a.publish(Event{Event: EventQueueUpdate, Steering: steering, FollowUp: followUp})
	_, _ = a.Send(text)
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

	a.publish(Event{Event: EventTurnEnd, TurnID: turnID, Reason: reason})
	a.publish(Event{Event: EventAgentEnd, TurnID: turnID, Reason: reason})
	a.publish(Event{Event: EventSettled, TurnID: turnID, Reason: reason})
	if ch != nil {
		close(ch)
	}
	if reason == ReasonDone {
		a.deliverQueued()
	}
}

// runTurn executes one full send -> model -> tools -> settle cycle.
func (a *Agent) runTurn(ctx context.Context, text string) {
	turnID := a.currentTurnID()
	a.publish(Event{Event: EventAgentStart, TurnID: turnID})
	a.publish(Event{Event: EventTurnStarted, TurnID: turnID, Text: text})

	defer func() {
		if r := recover(); r != nil {
			a.publish(Event{Event: EventError, Message: fmt.Sprintf("internal error: %v", r)})
			a.settle(ReasonError)
		}
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Everything the turn costs is attributed to one of these, so a slow turn
	// names its own cause rather than needing a guess.
	a.mu.Lock()
	turnAt := a.turnStart
	a.mu.Unlock()
	phase := diagnostics.Turn{At: turnAt, ContextWindow: contextWindowFor(a.opts.Model)}
	defer func() {
		phase.TotalMs = time.Since(turnAt).Milliseconds()
		a.mu.Lock()
		reason := a.settleReason
		a.mu.Unlock()
		if reason != "" && reason != ReasonDone {
			phase.Error = reason
		}
		a.diag.Record(phase)
	}()

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
	//
	// This runs alongside the turn rather than before it. Naming is a whole
	// extra provider round trip, and it used to be the first thing the turn
	// waited on, so the first token of the first message in a session arrived
	// one model call late for a title nobody was reading yet. The store's
	// Append is mutex-guarded, so writing the title while the turn streams is
	// safe, and the title simply appears a moment later in the sidebar.
	if !a.titleSet {
		a.titleSet = true
		// The title is read out of the history the turn already keeps, not by
		// re-reading the file. session.Name parses the whole transcript, which
		// on a long session is hundreds of milliseconds before the first token
		// for a boolean check.
		titleAt := time.Now()
		entries, titleErr := a.history.sinceAppends(a.opts.Store.Path())
		phase.HistoryReadMs += time.Since(titleAt).Milliseconds()
		if titleErr == nil && session.LastName(entries) == "" {
			done := make(chan struct{})
			a.mu.Lock()
			a.namingDone = done
			a.mu.Unlock()
			go func() {
				defer close(done)
				defer func() {
					a.mu.Lock()
					if a.namingDone == done {
						a.namingDone = nil
					}
					a.mu.Unlock()
				}()
				a.nameSession(ctx, text, userEntry.ID)
			}()
		}
	}

	// 3. The loop: model -> tools -> model until no tool calls remain.
	for iter := 1; ; iter++ {
		phase.Iterations = iter
		if iter > a.opts.MaxIterations {
			a.publish(Event{Event: EventError, Message: "max iterations reached"})
			a.settle(ReasonError)
			return
		}
		if ctx.Err() != nil {
			a.finishAborted(turnID, nil)
			return
		}

		// Auto-compaction: summarize older entries when the conversation
		// nears the model's context window. Best-effort: a failure here must
		// not kill the turn.
		if a.opts.Settings.Compaction.Enabled {
			didCompact, err := a.autoCompactIfNeeded(ctx)
			if didCompact {
				phase.Compacted = true
			}
			if err != nil {
				a.publish(Event{Event: EventError, Message: "compaction skipped: " + err.Error()})
			}
		}

		readAt := time.Now()
		entries, err := a.history.sinceAppends(a.opts.Store.Path())
		phase.HistoryReadMs += time.Since(readAt).Milliseconds()
		if err != nil {
			a.publish(Event{Event: EventError, Message: "failed to read session: " + err.Error()})
			a.settle(ReasonError)
			return
		}
		buildAt := time.Now()
		hist, err := messagesFromEntries(entries, a.systemPrompt)
		if err != nil {
			a.publish(Event{Event: EventError, Message: fmt.Sprintf("failed to build history: %v", err)})
			a.settle(ReasonError)
			return
		}
		req := provider.Request{Model: a.opts.Model, Messages: hist, Tools: a.specs, MaxTokens: a.opts.MaxTokens}
		phase.BuildMs = time.Since(buildAt).Milliseconds()

		requestAt := time.Now()
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

		out, stopReason, firstAt, err := a.consumeStream(ctx, turnID, stream, iter)
		if !firstAt.IsZero() {
			phase.FirstTokenMs = firstAt.Sub(requestAt).Milliseconds()
		}
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
		a.runToolCalls(ctx, turnID, out.ToolCalls, assistantEntry.ID)
		if ctx.Err() != nil {
			a.finishAborted(turnID, nil)
			return
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
// consumeStream reads a provider stream to completion. It also reports when the
// first event arrived, which is the only honest measure of time to first token.
func (a *Agent) consumeStream(ctx context.Context, turnID string, stream provider.Stream, iter int) (*turnOutput, string, time.Time, error) {
	var firstAt time.Time
	out := &turnOutput{}
	thinking := &thinkingFilter{}
	stopReason := "stop"
	a.publish(Event{Event: EventMessageStart, TurnID: turnID, Role: session.RoleAssistant, Iteration: iter})
	for {
		if firstAt.IsZero() {
			firstAt = time.Now()
		}
		ev, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, stopReason, firstAt, err
		}
		switch ev.Kind {
		case provider.EventText:
			if visible := thinking.Feed(ev.Text); visible != "" {
				out.Text.WriteString(visible)
				a.publish(Event{Event: EventMessageDelta, TurnID: turnID, Role: session.RoleAssistant, Text: visible})
			}
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
	if visible := thinking.Flush(); visible != "" {
		out.Text.WriteString(visible)
		a.publish(Event{Event: EventMessageDelta, TurnID: turnID, Role: session.RoleAssistant, Text: visible})
	}
	if ctx.Err() != nil {
		return out, stopReason, firstAt, ctx.Err()
	}
	return out, stopReason, firstAt, nil
}

// streamWithRetry calls the provider, retrying retryable errors with bounded
// backoff (max MaxRetries attempts).
func (a *Agent) streamWithRetry(ctx context.Context, req provider.Request) (provider.Stream, error) {
	a.mu.Lock()
	a.retryAbort = make(chan struct{})
	retryAbort := a.retryAbort
	a.mu.Unlock()
	r := a.opts.Settings.Retry
	if !r.Enabled {
		return a.opts.Provider.Stream(ctx, req)
	}
	maxAttempts := r.MaxRetries
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	baseDelay := r.BaseDelayMs
	if baseDelay <= 0 {
		baseDelay = 2000
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		s, err := a.opts.Provider.Stream(ctx, req)
		if err == nil {
			if attempt > 0 {
				a.publish(Event{Event: EventAutoRetryEnd, Attempt: attempt, Success: true})
			}
			return s, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt >= maxAttempts {
			a.publish(Event{Event: EventAutoRetryEnd, Attempt: attempt, FinalError: err.Error()})
			return nil, lastErr
		}
		var re *provider.RetryableError
		if !errors.As(err, &re) {
			return nil, lastErr
		}
		delay := time.Duration(baseDelay*(1<<uint(attempt))) * time.Millisecond
		a.publish(Event{Event: EventAutoRetryStart, Attempt: attempt + 1, MaxAttempts: maxAttempts, DelayMs: int(delay.Milliseconds()), ErrorMessage: err.Error()})
		select {
		case <-time.After(delay):
		case <-retryAbort:
			a.publish(Event{Event: EventAutoRetryEnd, Attempt: attempt + 1, Success: false, FinalError: "retry aborted"})
			return nil, fmt.Errorf("retry aborted")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// AbortRetry cancels a pending provider retry backoff.
func (a *Agent) AbortRetry() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.retryAbort != nil {
		select {
		case <-a.retryAbort:
		default:
			close(a.retryAbort)
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
		Provider:   "escape",
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

func (a *Agent) AnswerQuestion(id, answer string) error {
	a.mu.Lock()
	ch, ok := a.questions[id]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("question %q is not pending", id)
	}
	select {
	case ch <- answer:
		return nil
	default:
		return fmt.Errorf("question %q already has an answer", id)
	}
}

// AnswerApproval resolves a pending approval request for a side-effecting tool.
func (a *Agent) AnswerApproval(id string, approved bool) error {
	a.mu.Lock()
	ch, ok := a.approvals[id]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("approval %q is not pending", id)
	}
	select {
	case ch <- approved:
		return nil
	default:
		return fmt.Errorf("approval %q already has an answer", id)
	}
}

func (a *Agent) runQuestion(ctx context.Context, turnID string, tc provider.ToolCall) tools.Result {
	var args struct {
		Question string   `json:"question"`
		Choices  []string `json:"choices"`
	}
	if err := json.Unmarshal(mustJSON(tc.Args), &args); err != nil || strings.TrimSpace(args.Question) == "" {
		return tools.Result{Output: "question requires a non-empty question", IsError: true}
	}
	ch := make(chan string, 1)
	a.mu.Lock()
	a.questions[tc.ID] = ch
	a.mu.Unlock()
	a.publish(Event{Event: EventQuestionRequested, TurnID: turnID, ToolCallID: tc.ID, Name: tc.Name, Args: tc.Args, Question: args.Question, Choices: args.Choices})
	select {
	case answer := <-ch:
		return tools.Result{Output: answer}
	case <-ctx.Done():
		a.mu.Lock()
		delete(a.questions, tc.ID)
		a.mu.Unlock()
		return tools.Result{Output: "question cancelled", IsError: true}
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func (a *Agent) runToolCalls(ctx context.Context, turnID string, calls []provider.ToolCall, parentID string) {
	allParallel := true
	for _, tc := range calls {
		t, ok := a.reg.Get(tc.Name)
		if !ok || !t.ParallelSafe {
			allParallel = false
			break
		}
	}
	if !allParallel {
		for _, tc := range calls {
			a.runOneTool(ctx, turnID, tc, parentID)
		}
		return
	}
	var wg sync.WaitGroup
	for _, tc := range calls {
		wg.Add(1)
		go func(tc provider.ToolCall) {
			defer wg.Done()
			a.runOneTool(ctx, turnID, tc, parentID)
		}(tc)
	}
	wg.Wait()
}

func approvalSensitive(name string) bool {
	switch name {
	case "bash", "write", "edit":
		return true
	default:
		return false
	}
}

func (a *Agent) requestApproval(ctx context.Context, turnID string, tc provider.ToolCall) (bool, error) {
	if a.opts.ApprovalMode != ApprovalAsk || !approvalSensitive(tc.Name) {
		return true, nil
	}
	ch := make(chan bool, 1)
	a.mu.Lock()
	a.approvals[tc.ID] = ch
	a.mu.Unlock()
	a.publish(Event{Event: EventApprovalRequested, TurnID: turnID, ToolCallID: tc.ID, Name: tc.Name, Args: tc.Args})
	select {
	case approved := <-ch:
		a.mu.Lock()
		delete(a.approvals, tc.ID)
		a.mu.Unlock()
		return approved, nil
	case <-ctx.Done():
		a.mu.Lock()
		delete(a.approvals, tc.ID)
		a.mu.Unlock()
		return false, ctx.Err()
	}
}

// runOneTool executes one tool call, persists the toolResult entry and emits
// tool_call/tool_result events.
func (a *Agent) runOneTool(ctx context.Context, turnID string, tc provider.ToolCall, parentID string) {
	a.publish(Event{Event: EventToolCall, TurnID: turnID, ToolCallID: tc.ID, Name: tc.Name, Args: tc.Args})

	var res tools.Result
	approved, err := a.requestApproval(ctx, turnID, tc)
	switch {
	case err != nil:
		res = tools.Result{Output: "tool approval cancelled: " + err.Error(), IsError: true}
	case !approved:
		res = tools.Result{Output: "tool execution denied by user", IsError: true}
	case tc.Name == "question":
		res = a.runQuestion(ctx, turnID, tc)
		a.mu.Lock()
		delete(a.questions, tc.ID)
		a.mu.Unlock()
	default:
		if t, ok := a.reg.Get(tc.Name); ok {
			res = t.Run(ctx, tc.Args)
		} else {
			res = tools.Result{Output: fmt.Sprintf("unknown tool %q", tc.Name), IsError: true}
		}
	}

	// Large output is truncated on the way in, with the full text spilled to a
	// file the model is told how to search. Keeping it whole costs a session a
	// megabyte of disk and the same again in the parsed history cache, for
	// output that is stale the moment the turn moves on.
	stored := res.Output
	if !instructionShapedTool(tc.Name) {
		name := fmt.Sprintf("%s-%s.txt", a.opts.Store.ID(), tc.ID)
		stored, _, _ = session.SpillToolOutput(res.Output, a.opts.SpillDir, name)
	}

	msg := &session.Message{
		Role:       session.RoleToolResult,
		ToolCallID: tc.ID,
		ToolName:   tc.Name,
		Content:    []session.Block{{Type: session.BlockText, Text: stored}},
		IsError:    res.IsError,
		Timestamp:  session.NowMillis(),
	}
	_, err = a.opts.Store.Append(session.Entry{Type: session.TypeMessage, ParentID: parentID, Message: msg})
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
	// Meta-calls (naming, recap) must yield only the result. Models that
	// reason with inline <think>…</think> text would otherwise leak their
	// thinking into session titles and recap lines.
	return stripThinking(b.String()), nil
}

// StripThinking removes inline thinking blocks from model text output.
func StripThinking(s string) string { return stripThinking(s) }

// stripThinking removes inline thinking blocks from model text output.
func stripThinking(s string) string {
	for _, pair := range [][2]string{
		{"<think>", "</think>"},
		{"<thinking>", "</thinking>"},
		{"<reasoning>", "</reasoning>"},
		{"<antml:thinking>", "</antml:thinking>"},
	} {
		open, close := pair[0], pair[1]
		for {
			i := strings.Index(s, open)
			if i < 0 {
				break
			}
			j := strings.Index(s[i+len(open):], close)
			if j < 0 {
				s = s[:i]
				break
			}
			s = s[:i] + s[i+len(open)+j+len(close):]
		}
	}
	return strings.TrimSpace(s)
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
	a.waitForNaming(ctx)
	entries, err := a.history.sinceAppends(a.opts.Store.Path())
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
	s = stripThinking(s) // last gate: thinking must never reach the stored recap
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

// Model is the model the next request will use.
func (a *Agent) Model() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.opts.Model
}

// ToolCount is how many tools a request carries.
func (a *Agent) ToolCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.specs)
}

// HistoryLen is how many entries the session has been parsed into, which is the
// input to every request build.
func (a *Agent) HistoryLen() int {
	entries, err := a.history.sinceAppends(a.opts.Store.Path())
	if err != nil {
		return 0
	}
	return len(entries)
}

// waitForNaming blocks until an in-flight title write has landed, or the context
// is done. A recap that copies the title must not race the write, or it
// persists an empty one.
func (a *Agent) waitForNaming(ctx context.Context) {
	a.mu.Lock()
	done := a.namingDone
	a.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// SetCwd changes the directory the agent works in, and rebinds everything that
// was built for the previous one.
//
// Escape is spawned with a single --cwd, which used to mean the project was
// whatever directory the shell happened to be launched from. Choosing a project
// has to be able to change it, and three things are bound to it: the tools,
// their advertised specs, and the system prompt. Leaving any of them pointing at
// the old directory would mean editing files in one project while the model is
// told about another.
func (a *Agent) SetCwd(cwd string, toolSet []tools.Tool) {
	a.mu.Lock()
	if a.state == StateRunning {
		a.mu.Unlock()
		return
	}
	a.opts.Cwd = cwd
	reg := tools.New(toolSet...)
	a.reg = reg
	a.specs = reg.Specs()
	a.systemPrompt = buildSystemPrompt(a.opts)
	a.mu.Unlock()
}

// instructionShapedTool reports whether a tool's output is instructions to the
// model rather than an observation about the codebase.
//
// Truncating or pruning these is how an agent loses the rules it was given. A
// skill body is a set of instructions, and durable memory holds the decisions
// the person recorded; both are small and both are load-bearing, so they are
// exempt from both truncation and the compaction prune.
func instructionShapedTool(name string) bool {
	switch name {
	case "skill", "memory", "project_memory":
		return true
	default:
		return false
	}
}

// Cwd is the directory the agent is working in.
func (a *Agent) Cwd() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.opts.Cwd
}
