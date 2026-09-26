package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ekasc/escape/engine/internal/agent"
	"github.com/ekasc/escape/engine/internal/memory"
	"github.com/ekasc/escape/engine/internal/provider"
	"github.com/ekasc/escape/engine/internal/resources"
	"github.com/ekasc/escape/engine/internal/session"
	"github.com/ekasc/escape/engine/internal/settings"
	"github.com/ekasc/escape/engine/internal/tools"
)

// PiServer implements pi's command/event JSON-lines RPC protocol:
//
//	stdin  request:  {"type": "<command>", ...params, "id"?: ...}
//	stdout response: {"type": "response", "command": ..., "success": bool, "data"?: {...}}
//	stdout event:    {"type": "<eventName>", ...}
type PiServer struct {
	prov provider.Provider
	ts   []tools.Tool
	set  *settings.Settings
	cwd  string
	root string
	ctrl *session.Control

	mu     sync.Mutex
	agent  *agent.Agent
	store  *session.Store
	loader *resources.Loader

	outMu sync.Mutex
	out   io.Writer
	wg    sync.WaitGroup

	serveStarted bool
	serveCtx     context.Context
	relayCancel  context.CancelFunc
	relayDone    chan struct{}

	autoCompaction bool
	autoRetry      bool
	steeringMode   string
	followUpMode   string

	bashMu  sync.Mutex
	bashCmd *exec.Cmd
}

type piRequest struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	ID      string `json:"id"`

	Message    string   `json:"message"`
	Text       string   `json:"text"`
	Model      string   `json:"model"`
	Provider   string   `json:"provider"`
	APIKey     string   `json:"apiKey"`
	MaxTokens  int      `json:"maxTokens"`
	Level      string   `json:"level"`
	Mode       string   `json:"mode"`
	Name       string   `json:"name"`
	Cwd        string   `json:"cwd"`
	EntryID    string   `json:"entryId"`
	Since      string   `json:"since"`
	FromID     string   `json:"fromId"`
	TargetID   string   `json:"targetId"`
	Answer     string   `json:"answer"`
	Approved   *bool    `json:"approved"`
	QuestionID string   `json:"questionId"`
	ToolCallID string   `json:"toolCallId"`
	Path       string   `json:"sessionPath"`
	Output     string   `json:"outputPath"`
	Paths      []string `json:"paths"`

	Enabled            *bool  `json:"enabled"`
	CustomInstructions string `json:"customInstructions"`

	Op    string `json:"op"`
	Index int    `json:"index"`
	// Scope selects which list a skill decision belongs to: "global" applies to
	// every project, "project" applies to the project named below and beats the
	// global choice.
	Scope string `json:"scope"`
	// Project is the cwd a project-scoped decision applies to. It is named
	// rather than inferred, because the engine can hold more than one project
	// at a time and "the current one" is not a fact it can rely on.
	Project string `json:"project"`
}

type piResponse struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
	ID      string `json:"id,omitempty"`
}

// NewPiServer opens the session at sessionPath and builds the agent.
func NewPiServer(prov provider.Provider, ts []tools.Tool, set *settings.Settings, cwd, root, sessionPath string, ctrl *session.Control, out io.Writer) (*PiServer, error) {
	if set == nil {
		set = settings.Defaults()
	}
	s := &PiServer{
		prov: prov, ts: ts, set: set, cwd: cwd, root: root, ctrl: ctrl, out: out,
		autoCompaction: set.Compaction.Enabled,
		autoRetry:      set.Retry.Enabled,
		steeringMode:   set.SteeringMode,
		followUpMode:   set.FollowUpMode,
		loader:         resources.New(cwd, set),
	}
	if err := s.buildAgent(sessionPath); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *PiServer) startRelay(ctx context.Context) {
	s.stopRelay()
	events := s.agent.Events()
	relayCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.serveCtx = ctx
	s.relayCancel = cancel
	s.relayDone = done
	go func() {
		defer close(done)
		s.Relay(relayCtx, events)
	}()
}

func (s *PiServer) stopRelay() {
	if s.relayCancel == nil {
		return
	}
	s.relayCancel()
	if s.relayDone != nil {
		select {
		case <-s.relayDone:
		case <-time.After(100 * time.Millisecond):
		}
	}
	s.relayCancel = nil
	s.relayDone = nil
}

// applyPendingResume performs a session switch the agent asked for during the
// turn that has just settled. It must run here rather than inside the tool: the
// request comes from an agent that a synchronous switch would stop mid-call.
//
// The switch emits session_switched so the shell can follow the engine, and a
// failure is surfaced rather than swallowed: the model was told a resume was
// coming, so silently not doing it would be a lie.
func (s *PiServer) applyPendingResume() {
	path := s.ctrl.TakeResume()
	if path == "" {
		return
	}
	if err := s.buildAgent(path); err != nil {
		s.emit(map[string]any{"type": "error", "message": "could not resume session: " + err.Error()})
		return
	}
	info, _ := session.ReadInfo(path)
	s.emit(map[string]any{"type": "session_switched", "path": path, "session": info})
}

// skillViews is the wire shape for the skills list. Disabled skills are
// included with enabled=false rather than omitted, because the settings surface
// has to be able to show what the user switched off.
// knownProjects lists the projects a skill decision may be scoped to, newest
// activity first. Anything outside this set is refused rather than written to.
const maxKnownProjects = 50

type projectView struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

func (s *PiServer) knownProjects() ([]projectView, error) {
	paths, err := session.Projects(s.root, maxKnownProjects)
	if err != nil {
		return nil, err
	}
	out := make([]projectView, 0, len(paths))
	for _, p := range paths {
		out = append(out, projectView{Path: p, Name: filepath.Base(p)})
	}
	return out, nil
}

// skillViews reports, per skill, the global decision and any per-project
// decision that exists.
//
// Decisions belong to projects, not to whichever one this process sits in, so
// each project's own settings file is read for the project-scoped truth. What is
// not reported is an effective value per project, because that is derived: the
// surface combines a project's override with the global list, and doing the
// merge in two places is how they drift apart.
func (s *PiServer) skillViews(skills []resources.Skill, projects []projectView) []map[string]any {
	overridesByProject := make(map[string]map[string]bool, len(projects))
	for _, project := range projects {
		set, err := settings.Load(project.Path)
		if err != nil {
			continue
		}
		overrides := make(map[string]bool, len(set.SkillOverrides))
		for _, o := range set.SkillOverrides {
			overrides[filepath.Clean(strings.TrimSpace(o.Path))] = o.Enabled
		}
		overridesByProject[filepath.Clean(project.Path)] = overrides
	}

	globallyDisabled := make(map[string]bool, len(s.set.DisabledSkills))
	for _, p := range s.set.DisabledSkills {
		globallyDisabled[filepath.Clean(strings.TrimSpace(p))] = true
	}

	out := make([]map[string]any, 0, len(skills))
	for _, skill := range skills {
		path := filepath.Clean(skill.Path)
		overrides := make([]map[string]any, 0, 2)
		for _, project := range projects {
			if enabled, ok := overridesByProject[filepath.Clean(project.Path)][path]; ok {
				overrides = append(overrides, map[string]any{"project": project.Path, "enabled": enabled})
			}
		}
		// The project that owns a skill, when it is not a user level one. A
		// project skill cannot be decided anywhere else, so the surface needs to
		// know that rather than offer a choice that would do nothing.
		owner := ""
		if skill.Location == "project" {
			for _, project := range projects {
				if strings.HasPrefix(skill.Path, filepath.Clean(project.Path)+string(filepath.Separator)) {
					owner = project.Path
					break
				}
			}
		}
		out = append(out, map[string]any{
			"name":          skill.Name,
			"description":   skill.Description,
			"path":          skill.Path,
			"location":      skill.Location,
			"enabled":       !skill.Disabled,
			"globalEnabled": !globallyDisabled[path],
			"project":       owner,
			"overrides":     overrides,
		})
	}
	return out
}

// handleSetSkillEnabled records a skill decision in one of the two lists.
//
// The global list applies to every project. A project decision beats it, and
// clearing a project decision returns that skill to inheriting the global one,
// which is why enabled is a pointer: absent means inherit, not off.
//
// The system prompt is fixed when an agent is built, so a change only reaches
// the model on a rebuild. Doing that mid-turn would stop the turn the user is
// watching, so a busy engine records the change, answers pending, and the
// surface says so.
func (s *PiServer) handleSetSkillEnabled(req piRequest, cmd string) {
	path := filepath.Clean(strings.TrimSpace(req.Path))
	if path == "" {
		s.finish(req, cmd, nil, fmt.Errorf("skill path is required"))
		return
	}
	known := false
	for _, skill := range s.loader.Skills() {
		if filepath.Clean(skill.Path) == path {
			known = true
			break
		}
	}
	if !known {
		s.finish(req, cmd, nil, fmt.Errorf("no skill at %s", path))
		return
	}
	scope := strings.TrimSpace(req.Scope)
	if scope == "" {
		scope = "global"
	}
	if scope != "global" && scope != "project" {
		s.finish(req, cmd, nil, fmt.Errorf("scope must be global or project"))
		return
	}
	// A project scope names its project. The engine can be looking at more than
	// one at a time, and accepting an arbitrary path here would turn this into a
	// write to any directory the process can reach.
	project := ""
	if scope == "project" {
		known, err := s.knownProjects()
		if err != nil {
			s.finish(req, cmd, nil, err)
			return
		}
		want := filepath.Clean(strings.TrimSpace(req.Project))
		for _, p := range known {
			if filepath.Clean(p.Path) == want {
				project = p.Path
				break
			}
		}
		if project == "" {
			s.finish(req, cmd, nil, fmt.Errorf("%s is not a known project", want))
			return
		}
	}

	s.mu.Lock()
	if scope == "global" {
		disabled := make([]string, 0, len(s.set.DisabledSkills)+1)
		for _, p := range s.set.DisabledSkills {
			if filepath.Clean(strings.TrimSpace(p)) != path {
				disabled = append(disabled, p)
			}
		}
		// A nil enabled clears the global decision, so the skill goes back to on
		// everywhere unless a project says otherwise.
		if req.Enabled != nil && !*req.Enabled {
			disabled = append(disabled, path)
		}
		sort.Strings(disabled)
		s.set.DisabledSkills = disabled
	} else {
		// The merged settings only carry this process's own project overrides, so
		// another project's list has to be read from that project's file.
		existing, err := settings.Load(project)
		if err != nil {
			s.mu.Unlock()
			s.finish(req, cmd, nil, err)
			return
		}
		overrides := make([]settings.SkillOverride, 0, len(existing.SkillOverrides)+1)
		for _, o := range existing.SkillOverrides {
			if filepath.Clean(strings.TrimSpace(o.Path)) != path {
				overrides = append(overrides, o)
			}
		}
		if req.Enabled != nil {
			overrides = append(overrides, settings.SkillOverride{Path: path, Enabled: *req.Enabled})
		}
		sort.Slice(overrides, func(i, j int) bool { return overrides[i].Path < overrides[j].Path })
		s.set.SkillOverrides = overrides
	}
	live := s.agent
	s.mu.Unlock()

	var writeErr error
	if scope == "global" {
		s.mu.Lock()
		disabled := append([]string{}, s.set.DisabledSkills...)
		s.mu.Unlock()
		writeErr = settings.SetDisabledSkills(disabled)
	} else {
		s.mu.Lock()
		overrides := append([]settings.SkillOverride{}, s.set.SkillOverrides...)
		s.mu.Unlock()
		writeErr = settings.SetSkillOverrides(project, overrides)
	}
	if writeErr != nil {
		s.finish(req, cmd, nil, writeErr)
		return
	}

	pending := false
	if live != nil && live.State().State == agent.StateRunning {
		pending = true
	} else if err := s.buildAgent(s.store.Path()); err != nil {
		s.finish(req, cmd, nil, err)
		return
	}
	// The loader caches its skills and has no invalidation, so a decision has to
	// replace it. Without this the command list and the settings list keep
	// serving the previous state, which puts a disabled skill's description
	// back into get_commands.
	s.mu.Lock()
	s.loader = resources.New(s.cwd, s.set)
	s.mu.Unlock()

	s.finish(req, cmd, map[string]any{
		"path":    path,
		"scope":   scope,
		"enabled": req.Enabled,
		"pending": pending,
	}, nil)
}

// handleMemory exposes the project's durable store to the shell so the user can
// see and correct what the agent chose to remember. The same store backs the
// memory tool, so an edit here shows up in the next session's snapshot.
func (s *PiServer) handleMemory(req piRequest, cmd string) {
	store := memory.Open(s.root, s.cwd)
	var err error
	switch req.Op {
	case "", "list":
	case "add":
		err = store.Add(req.Text)
	case "replace":
		err = store.Replace(req.Index, req.Text)
	case "remove":
		err = store.Remove(req.Index)
	default:
		s.finish(req, cmd, nil, fmt.Errorf("unknown memory op %q", req.Op))
		return
	}
	if err != nil {
		s.finish(req, cmd, nil, err)
		return
	}
	// One read, so the entries and the size in this response agree.
	entries, used, readErr := store.Snapshot()
	if readErr != nil {
		s.finish(req, cmd, nil, readErr)
		return
	}
	s.finish(req, cmd, map[string]any{
		"entries": entries,
		"used":    used,
		"max":     memory.MaxChars,
		"path":    store.Path(),
		"project": filepath.Clean(s.cwd),
	}, nil)
}

func (s *PiServer) buildAgent(sessionPath string) error {
	path, err := filepath.Abs(sessionPath)
	if err != nil {
		return err
	}
	store, err := session.Open(path, s.cwd)
	if err != nil {
		return err
	}
	oldAgent := s.agent
	oldStore := s.store
	ag := agent.New(agent.Options{
		Store:        store,
		Provider:     s.prov,
		Tools:        s.ts,
		Cwd:          s.cwd,
		Model:        s.model(),
		Settings:     s.set,
		ApprovalMode: agent.ApprovalMode(s.set.ApprovalMode),
		// Read once per agent build: a session sees a frozen snapshot, and
		// switching sessions picks up whatever the agent wrote last time.
		Memory: memory.Open(s.root, s.cwd).Render(),
	})
	if oldAgent != nil {
		oldAgent.Stop()
		oldAgent.Wait()
	}
	s.agent = ag
	s.store = store
	s.ctrl.SetCurrent(path)
	if oldStore != nil {
		_ = oldStore.Close()
	}
	if s.serveStarted {
		s.startRelay(s.serveCtx)
	}
	return nil
}

func (s *PiServer) model() string {
	return s.set.DefaultModel
}

func (s *PiServer) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	s.out.Write(append(b, '\n'))
}

func (s *PiServer) respond(id, command string, success bool, data any, errMsg string) {
	r := piResponse{Type: "response", Command: command, Success: success, Data: data, Error: errMsg, ID: id}
	s.write(r)
}

func (s *PiServer) emit(m map[string]any) {
	s.write(m)
}

// Relay translates agent events into pi wire events.
func (s *PiServer) Relay(ctx context.Context, events <-chan agent.Event) {
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			s.translate(ev)
		case <-ctx.Done():
			return
		}
	}
}

func (s *PiServer) translate(ev agent.Event) {
	switch ev.Event {
	case agent.EventAgentStart:
		s.emit(map[string]any{"type": "agent_start"})
	case agent.EventTurnStarted:
		s.emit(map[string]any{"type": "turn_start"})
	case agent.EventMessageStart:
		if ev.Role == session.RoleAssistant {
			s.emit(map[string]any{"type": "message_start"})
		}
	case agent.EventMessageDelta:
		if ev.Role == session.RoleAssistant {
			s.emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": ev.Text}})
		}
	case agent.EventMessageEnd:
		if ev.Role == session.RoleAssistant {
			m := map[string]any{"type": "message_end"}
			if ev.Model != "" {
				m["model"] = ev.Model
			}
			s.emit(m)
		}
	case agent.EventToolCall:
		s.emit(map[string]any{"type": "tool_execution_start", "toolCallId": ev.ToolCallID, "toolName": ev.Name, "args": ev.Args})
	case agent.EventToolResult:
		s.emit(map[string]any{"type": "tool_execution_end", "toolCallId": ev.ToolCallID, "toolName": ev.Name,
			"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": ev.Output}}, "details": map[string]any{}}, "isError": ev.Error})
	case agent.EventTurnEnd:
		s.emit(map[string]any{"type": "turn_end"})
	case agent.EventAgentEnd:
		s.emit(map[string]any{"type": "agent_end"})
	case agent.EventSettled:
		s.emit(map[string]any{"type": "agent_settled"})
		s.applyPendingResume()
	case agent.EventQuestionRequested:
		s.emit(map[string]any{"type": "question_requested", "questionId": ev.ToolCallID, "question": ev.Question, "choices": ev.Choices})
	case agent.EventApprovalRequested:
		s.emit(map[string]any{"type": "approval_requested", "toolCallId": ev.ToolCallID, "toolName": ev.Name, "args": ev.Args})
	case agent.EventError:
		s.emit(map[string]any{"type": "error", "message": ev.Message})
	case agent.EventCompactionStart:
		s.emit(map[string]any{"type": "compaction_start", "reason": ev.Reason})
	case agent.EventCompactionEnd:
		s.emit(map[string]any{"type": "compaction_end", "reason": ev.Reason, "result": map[string]any{
			"summary": ev.Summary, "firstKeptEntryId": ev.FirstKeptEntryID, "tokensBefore": ev.TokensBefore, "estimatedTokensAfter": ev.EstimatedTokensAfter}})
	case agent.EventAutoRetryStart:
		s.emit(map[string]any{"type": "auto_retry_start", "attempt": ev.Attempt, "maxAttempts": ev.MaxAttempts, "delayMs": ev.DelayMs, "errorMessage": ev.ErrorMessage})
	case agent.EventAutoRetryEnd:
		m := map[string]any{"type": "auto_retry_end", "success": ev.Success, "attempt": ev.Attempt}
		if ev.FinalError != "" {
			m["finalError"] = ev.FinalError
		}
		s.emit(m)
	case agent.EventQueueUpdate:
		s.emit(map[string]any{"type": "queue_update", "steering": ev.Steering, "followUp": ev.FollowUp})
	}
}

// Serve reads requests until stdin closes, dispatching each in a goroutine.
func (s *PiServer) Serve(ctx context.Context, in io.Reader) error {
	s.serveStarted = true
	s.startRelay(ctx)
	defer func() {
		s.serveStarted = false
		s.stopRelay()
	}()

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req piRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			s.respond("", "parse", false, nil, err.Error())
			continue
		}
		cmd := req.Type
		if cmd == "command" {
			cmd = req.Command
		}
		s.wg.Add(1)
		go func(cmd string, req piRequest) {
			defer s.wg.Done()
			s.dispatch(ctx, cmd, req)
		}(cmd, req)
	}

	s.agent.Stop()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
	}
	return nil
}

func (s *PiServer) dispatch(ctx context.Context, cmd string, req piRequest) {
	switch cmd {
	case "prompt":
		turnID, err := s.agent.Send(req.Message)
		s.finish(req, cmd, map[string]any{"turnId": turnID}, err)
	case "steer":
		turnID, err := s.agent.QueueSteer(req.Text)
		s.finish(req, cmd, map[string]any{"turnId": turnID}, err)
	case "follow_up":
		turnID, err := s.agent.FollowUp(req.Text)
		s.finish(req, cmd, map[string]any{"turnId": turnID}, err)
	case "abort":
		s.agent.Stop()
		s.finish(req, cmd, map[string]any{}, nil)
	case "set_model":
		if req.Model == "" {
			s.finish(req, cmd, nil, fmt.Errorf("model required"))
			return
		}
		s.agent.SetModel(req.Model)
		s.mu.Lock()
		s.set.DefaultModel = req.Model
		s.mu.Unlock()
		s.finish(req, cmd, map[string]any{}, nil)
	case "set_max_tokens":
		if req.MaxTokens < 0 {
			s.finish(req, cmd, nil, fmt.Errorf("maxTokens must be non-negative"))
			return
		}
		s.agent.SetMaxTokens(req.MaxTokens)
		s.finish(req, cmd, map[string]any{"maxTokens": req.MaxTokens}, nil)
	case "set_thinking_level":
		s.agent.SetThinkingLevel(req.Level)
		s.mu.Lock()
		s.set.DefaultThinkingLevel = req.Level
		s.mu.Unlock()
		s.finish(req, cmd, map[string]any{}, nil)
	case "set_api_key":
		if req.Provider != "opencode-go" && req.Provider != "opencode-zen" {
			s.finish(req, cmd, nil, fmt.Errorf("API-key login is supported for opencode-go and opencode-zen"))
			return
		}
		err := provider.SaveOpenCodeKey(req.Provider, req.APIKey)
		s.finish(req, cmd, map[string]any{"provider": req.Provider}, err)
	case "set_approval_mode":
		if req.Mode != "auto" && req.Mode != "ask" {
			s.finish(req, cmd, nil, fmt.Errorf("approval mode must be auto or ask"))
			return
		}
		if err := s.agent.SetApprovalMode(agent.ApprovalMode(req.Mode)); err != nil {
			s.finish(req, cmd, nil, err)
			return
		}
		s.mu.Lock()
		s.set.ApprovalMode = req.Mode
		s.mu.Unlock()
		s.finish(req, cmd, map[string]any{"approvalMode": req.Mode}, nil)
	case "get_providers":
		s.finish(req, cmd, map[string]any{"providers": s.providerSummaries()}, nil)
	case "set_provider":
		p, err := s.providerFor(req.Provider, req.Model)
		if err != nil {
			s.finish(req, cmd, nil, err)
			return
		}
		s.mu.Lock()
		s.prov = p
		s.set.DefaultProvider = req.Provider
		if req.Model != "" {
			s.set.DefaultModel = req.Model
		}
		s.mu.Unlock()
		s.agent.SetProvider(p)
		if req.Model != "" {
			s.agent.SetModel(req.Model)
		}
		s.finish(req, cmd, map[string]any{"provider": req.Provider}, nil)
	case "get_available_thinking_levels":
		s.finish(req, cmd, map[string]any{"levels": []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}}, nil)
	case "set_steering_mode":
		if req.Mode != "" {
			s.mu.Lock()
			s.steeringMode = req.Mode
			s.mu.Unlock()
		}
		s.finish(req, cmd, map[string]any{}, nil)
	case "set_follow_up_mode":
		if req.Mode != "" {
			s.mu.Lock()
			s.followUpMode = req.Mode
			s.mu.Unlock()
		}
		s.finish(req, cmd, map[string]any{}, nil)
	case "list_skills":
		projects, err := s.knownProjects()
		if err != nil {
			s.finish(req, cmd, nil, err)
			return
		}
		s.finish(req, cmd, map[string]any{
			"skills":   s.skillViews(s.loader.Skills(), projects),
			"projects": projects,
		}, nil)
	case "set_skill_enabled":
		s.handleSetSkillEnabled(req, cmd)
	case "memory":
		s.handleMemory(req, cmd)
	case "answer_question":
		err := s.agent.AnswerQuestion(req.QuestionID, req.Answer)
		s.finish(req, cmd, map[string]any{}, err)
	case "answer_approval":
		if req.Approved == nil {
			s.finish(req, cmd, nil, fmt.Errorf("approved is required"))
			return
		}
		err := s.agent.AnswerApproval(req.ToolCallID, *req.Approved)
		s.finish(req, cmd, map[string]any{}, err)
	case "snapcompact":
		res, err := s.agent.Snapcompact(ctx)
		if err != nil {
			s.finish(req, cmd, nil, err)
			return
		}
		s.finish(req, cmd, map[string]any{"summary": res.Summary, "firstKeptEntryId": res.FirstKeptEntryID, "tokensBefore": res.TokensBefore, "estimatedTokensAfter": res.EstimatedTokensAfter}, nil)
	case "recap":
		text, err := s.agent.Recap(ctx)
		s.finish(req, cmd, map[string]any{"recap": text}, err)
	case "compact":
		res, err := s.agent.Compact(ctx, req.CustomInstructions)
		if err != nil {
			s.finish(req, cmd, nil, err)
			return
		}
		s.finish(req, cmd, map[string]any{"summary": res.Summary, "firstKeptEntryId": res.FirstKeptEntryID, "tokensBefore": res.TokensBefore, "estimatedTokensAfter": res.EstimatedTokensAfter}, nil)
	case "set_auto_compaction":
		if req.Enabled != nil {
			s.mu.Lock()
			s.autoCompaction = *req.Enabled
			s.mu.Unlock()
		}
		s.finish(req, cmd, map[string]any{}, nil)
	case "set_auto_retry":
		if req.Enabled != nil {
			s.mu.Lock()
			s.autoRetry = *req.Enabled
			s.set.Retry.Enabled = *req.Enabled
			s.mu.Unlock()
		}
		s.finish(req, cmd, map[string]any{}, nil)
	case "abort_retry":
		s.agent.AbortRetry()
		s.finish(req, cmd, map[string]any{}, nil)
	case "bash":
		s.handleBash(req, cmd)
	case "abort_bash":
		s.bashMu.Lock()
		if s.bashCmd != nil {
			_ = s.bashCmd.Process.Kill()
		}
		s.bashMu.Unlock()
		s.finish(req, cmd, map[string]any{}, nil)
	case "get_state":
		s.finish(req, cmd, s.getState(), nil)
	case "get_session_stats":
		ctxw := session.LookupModel(s.model()).ContextWindow
		stats, err := session.Stats(s.store.Path(), ctxw)
		s.finish(req, cmd, stats, err)
	case "diff":
		text, err := session.WorkspaceDiff(s.cwd, req.Paths...)
		s.finish(req, cmd, map[string]any{"diff": text}, err)
	case "export_html":
		out := req.Output
		if out == "" {
			out = filepath.Join(os.TempDir(), "session-"+time.Now().Format("20060102-150405")+".html")
		}
		err := session.ExportHTML(s.store.Path(), out)
		s.finish(req, cmd, map[string]any{"path": out}, err)
	case "list_sessions":
		// ListForCwd probes each session header and only pays the full read for
		// this project, rather than reading every session in the store and
		// discarding all but a handful.
		filtered, err := session.ListForCwd(s.root, s.cwd)
		if err != nil {
			s.finish(req, cmd, nil, err)
			return
		}
		s.finish(req, cmd, map[string]any{"sessions": filtered}, nil)
	case "new_session":
		path, err := session.NewPath(s.root, s.cwd)
		if err == nil {
			err = s.buildAgent(path)
		}
		if err != nil {
			s.finish(req, cmd, nil, err)
			return
		}
		info, _ := session.ReadInfo(path)
		s.finish(req, cmd, map[string]any{"path": path, "session": info}, nil)
	case "switch_session":
		if req.Path == "" {
			s.finish(req, cmd, nil, fmt.Errorf("sessionPath required"))
			return
		}
		err := s.buildAgent(req.Path)
		var info *session.Info
		if err == nil {
			info, _ = session.ReadInfo(req.Path)
		}
		s.finish(req, cmd, map[string]any{"path": req.Path, "session": info, "cancelled": false}, err)
	case "undo":
		newPath, err := session.UndoPath(s.store.Path(), s.cwd, s.root)
		if err == nil {
			err = s.buildAgent(newPath)
		}
		var info *session.Info
		if err == nil {
			info, _ = session.ReadInfo(newPath)
		}
		s.finish(req, cmd, map[string]any{"path": newPath, "session": info}, err)
	case "fork":
		newPath, text, err := session.Fork(s.store.Path(), req.EntryID, s.cwd)
		s.finish(req, cmd, map[string]any{"text": text, "path": newPath}, err)
	case "clone":
		newPath, err := session.Clone(s.store.Path(), s.cwd)
		s.finish(req, cmd, map[string]any{"path": newPath}, err)
	case "get_fork_messages":
		msgs, err := session.GetForkMessages(s.store.Path())
		s.finish(req, cmd, map[string]any{"messages": msgs}, err)
	case "get_entries":
		// An explicit sessionPath lets the shell read another session's
		// transcript without switching the engine's active (and possibly
		// running) session.
		readPath := s.store.Path()
		if req.Path != "" {
			readPath = req.Path
		}
		entries, leaf, err := session.EntriesSince(readPath, req.Since)
		s.finish(req, cmd, map[string]any{"entries": entries, "leafId": leaf}, err)
	case "get_tree":
		tree, leaf, err := session.Tree(s.store.Path())
		s.finish(req, cmd, map[string]any{"tree": tree, "leafId": leaf}, err)
	case "get_last_assistant_text":
		text, err := session.LastAssistantText(s.store.Path())
		s.finish(req, cmd, map[string]any{"text": text}, err)
	case "set_session_name":
		err := session.SetName(s.store.Path(), req.Name)
		s.finish(req, cmd, map[string]any{}, err)
	case "get_commands":
		s.finish(req, cmd, map[string]any{"commands": s.loader.GetCommands()}, nil)
	case "get_models":
		s.finish(req, cmd, map[string]any{"models": modelCatalog()}, nil)
	case "ping":
		s.finish(req, cmd, map[string]any{"pong": true}, nil)
	default:
		s.finish(req, cmd, nil, fmt.Errorf("unknown command: %s", cmd))
	}
}

func (s *PiServer) finish(req piRequest, cmd string, data any, err error) {
	if err != nil {
		s.respond(req.ID, cmd, false, nil, err.Error())
		return
	}
	s.respond(req.ID, cmd, true, data, "")
}

func (s *PiServer) providerSummaries() []map[string]any {
	_, codexConfigured := provider.ChatGPTTokens()
	return []map[string]any{
		{"id": "api", "name": "OpenAI-compatible API", "configured": os.Getenv("ESCAPE_API_KEY") != ""},
		{"id": "opencode-go", "name": "OpenCode Go", "configured": provider.OpenCodeGoKey() != ""},
		{"id": "opencode-zen", "name": "OpenCode Zen", "configured": provider.OpenCodeZenKey() != ""},
		{"id": "codex", "name": "Codex", "configured": codexConfigured == nil},
	}
}

func (s *PiServer) providerFor(name, model string) (provider.Provider, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "api":
		key := os.Getenv("ESCAPE_API_KEY")
		if key == "" {
			return nil, fmt.Errorf("provider api: set ESCAPE_API_KEY")
		}
		if model == "" {
			model = "gpt-4o-mini"
		}
		return provider.NewOpenAI(envOr("ESCAPE_BASE_URL", "https://api.openai.com/v1"), key, model), nil
	case "opencode-go":
		key := provider.OpenCodeGoKey()
		if key == "" {
			return nil, fmt.Errorf("provider opencode-go: no credential; run escape login")
		}
		if model == "" {
			model = "minimax-m3"
		}
		model = strings.TrimPrefix(model, "opencode-go/")
		return provider.NewOpenCodeGo(key, model), nil
	case "opencode-zen", "opencode":
		if model == "" {
			model = provider.OpenCodeZenDefaultModel
		}
		model = strings.TrimPrefix(model, "opencode-zen/")
		model = strings.TrimPrefix(model, "opencode/")
		return provider.NewOpenCodeZen(provider.OpenCodeZenKey(), model), nil
	case "codex", "openai", "chatgpt":
		tokens, err := provider.ChatGPTTokens()
		if err != nil {
			return nil, fmt.Errorf("provider codex: run escape login: %w", err)
		}
		if model == "" {
			model = "gpt-5.6-luna"
		}
		return provider.NewChatGPT(tokens, model), nil
	default:
		return nil, fmt.Errorf("unknown provider %q", name)
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func (s *PiServer) getState() map[string]any {
	info, _ := session.ReadInfo(s.store.Path())
	st := s.agent.State()
	m := map[string]any{
		"sessionId":     s.store.ID(),
		"sessionFile":   s.store.Path(),
		"cwd":           s.cwd,
		"model":         st.Model,
		"provider":      s.set.DefaultProvider,
		"thinkingLevel": s.set.DefaultThinkingLevel,
		"approvalMode":  st.ApprovalMode,
		"maxTokens":     s.agent.MaxTokens(),
		"state":         st.State,
	}
	if info != nil {
		m["sessionName"] = info.Name
	}
	s.mu.Lock()
	m["autoCompaction"] = s.autoCompaction
	m["autoRetry"] = s.autoRetry
	m["steeringMode"] = s.steeringMode
	m["followUpMode"] = s.followUpMode
	s.mu.Unlock()
	return m
}

// chunkWriter streams direct-bash output as bash_execution_update events.
type chunkWriter struct {
	s   *PiServer
	id  string
	buf strings.Builder
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	w.s.emit(map[string]any{"type": "bash_execution_update", "id": w.id, "delta": string(p)})
	return len(p), nil
}

func (s *PiServer) handleBash(req piRequest, cmd string) {
	execCmd := exec.Command("bash", "-lc", req.Command)
	execCmd.Dir = s.cwd
	execCmd.Env = os.Environ()
	id := req.ID
	if id == "" {
		id = fmt.Sprintf("bash-%d", time.Now().UnixNano())
	}
	w := &chunkWriter{s: s, id: id}
	execCmd.Stdout = w
	execCmd.Stderr = w

	s.bashMu.Lock()
	s.bashCmd = execCmd
	s.bashMu.Unlock()
	err := execCmd.Run()
	s.bashMu.Lock()
	s.bashCmd = nil
	s.bashMu.Unlock()

	exitCode := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	}
	data := map[string]any{"output": w.buf.String(), "exitCode": exitCode, "cancelled": false, "truncated": false}
	if err != nil && exitCode == 0 {
		s.respond(req.ID, cmd, false, nil, err.Error())
		return
	}
	s.respond(req.ID, cmd, true, data, "")
}

// modelCatalog is a small static catalog for get_models (id -> name).
func modelCatalog() []map[string]any {
	var ids []string
	for _, id := range openCodeGoModels {
		ids = append(ids, "opencode-go/"+id)
	}
	for _, id := range openCodeZenModels {
		ids = append(ids, "opencode-zen/"+id)
	}
	for _, id := range openAIModels {
		ids = append(ids, "openai/"+id)
	}
	sort.Strings(ids)
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		prov := "opencode-go"
		mid := id
		if i := strings.Index(id, "/"); i >= 0 {
			prov = id[:i]
			mid = id[i+1:]
		}
		out = append(out, map[string]any{"id": mid, "provider": prov, "name": titleCase(mid)})
	}
	return out
}

func titleCase(s string) string {
	parts := strings.Split(s, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// openCodeGoModels, openCodeZenModels, and openAIModels mirror the desktop shell's curated picker lists.
var openCodeGoModels = []string{
	"deepseek-v4-flash", "deepseek-v4-pro", "glm-5.1", "glm-5.2", "glm-5.3",
	"gpt-5.6-luna", "grok-4.5", "hy3", "kimi-k2.6", "kimi-k2.7-code", "kimi-k3",
	"mimo-v2.5", "mimo-v2.5-pro", "minimax-m2.7", "minimax-m3", "qwen3.6-plus",
	"qwen3.7-max", "qwen3.7-plus", "qwen3.8-max",
}

var openCodeZenModels = []string{
	"big-pickle", "ling-3.0-flash-fin-free", "mimo-v2.6-flash-free",
	"muse-spark-1.3-contributor-free", "nemotron-3-ultra-free",
	"nemotron-3.5-lightning-free", "space-bunny-free",
}

var openAIModels = []string{"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra"}
