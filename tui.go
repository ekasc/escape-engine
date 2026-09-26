package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/ekasc/escape/engine/internal/agent"
	"github.com/ekasc/escape/engine/internal/resources"
	"github.com/ekasc/escape/engine/internal/session"
	"github.com/ekasc/escape/engine/internal/settings"
)

const tuiCommandAreaHeight = 5

type tuiCommand struct {
	name         string
	description  string
	argumentHint string
}

const tuiReviewPrompt = "Review the current uncommitted changes in the workspace. Do not modify files. Identify bugs, regressions, security issues, and missing tests. Start with a concise summary and cite relevant file paths."

func tuiReviewPromptFor(paths ...string) string {
	if len(paths) == 0 {
		return tuiReviewPrompt
	}
	return tuiReviewPrompt + " Focus on these paths: " + strings.Join(paths, ", ") + "."
}

var tuiCommands = []tuiCommand{
	{name: "/help", description: "show available commands"},
	{name: "/commands", description: "list all available commands"},
	{name: "/login", description: "open provider login from a terminal"},
	{name: "/model", description: "show or set the model"},
	{name: "/reasoning", description: "show or set reasoning effort"},
	{name: "/state", description: "show agent state"},
	{name: "/status", description: "show session usage"},
	{name: "/diff", description: "show uncommitted changes"},
	{name: "/review", description: "review uncommitted changes"},
	{name: "/permissions", description: "show or set approval mode"},
	{name: "/rename", description: "set the session name"},
	{name: "/recap", description: "summarize recent session progress"},
	{name: "/export", description: "export the active session"},
	{name: "/clear", description: "clear the visible transcript"},
	{name: "/undo", description: "undo the latest user turn"},
	{name: "/fork", description: "branch from a user message"},
	{name: "/forks", description: "list user-message fork points"},
	{name: "/followup", description: "queue work after this turn"},
	{name: "/sessions", description: "list or switch sessions"},
	{name: "/new", description: "start a fresh session"},
	{name: "/stop", description: "stop the current turn"},
	{name: "/compact", description: "compact the conversation"},
	{name: "/snapcompact", description: "compact with recent context retained"},
	{name: "/exit", description: "quit escape"},
}

var (
	tuiUserStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	tuiToolStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	tuiMutedStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	tuiErrorStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	tuiSelectedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Background(lipgloss.Color("62"))
	tuiStatusStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	tuiBusyStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Bold(true)
	tuiSeparatorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
)

type tuiModel struct {
	agent          *agent.Agent
	events         <-chan agent.Event
	store          *session.Store
	sessionItems   []session.Info
	resourceLoader *resources.Loader
	input          textarea.Model
	transcript     viewport.Model
	spinner        spinner.Model
	lines          []string

	cwd                    string
	sessionRoot            string
	width                  int
	height                 int
	busy                   bool
	status                 string
	model                  string
	reasoning              string
	approvalMode           string
	history                []string
	historyPos             int
	questionID             string
	approvalID             string
	streamLine             int
	suggestions            []tuiCommand
	commandPos             int
	suggestionValue        string
	suggestionsInitialized bool
	loginForm              *huh.Form
	loginSelection         string
}

type tuiEventMsg struct{ event agent.Event }
type tuiSendMsg struct{ err error }
type tuiAnswerMsg struct{ err error }
type tuiApprovalMsg struct{ err error }
type tuiControlMsg struct{ text string }
type tuiSwitchSessionMsg struct {
	store *session.Store
	text  string
	err   error
}
type tuiExportMsg struct {
	text string
	err  error
}
type tuiClosedMsg struct{}

func newTUIModel(ag *agent.Agent, events <-chan agent.Event, model, reasoning string, initial ...[]session.Entry) *tuiModel {
	input := textarea.New()
	input.Placeholder = "Ask escape to build, inspect, or fix something"
	input.SetVirtualCursor(true)
	input.SetWidth(80)
	input.SetHeight(1)
	input.MinHeight = 1
	input.MaxHeight = 8
	input.DynamicHeight = true
	input.ShowLineNumbers = false
	input.KeyMap.InsertNewline.SetEnabled(false)
	inputStyles := input.Styles()
	inputStyles.Focused.CursorLine = lipgloss.NewStyle()
	input.SetStyles(inputStyles)
	input.Focus()

	transcript := viewport.New(viewport.WithWidth(80), viewport.WithHeight(16))
	transcript.SoftWrap = true
	transcript.MouseWheelEnabled = true
	transcript.SetHorizontalStep(0)

	m := &tuiModel{
		agent:        ag,
		events:       events,
		input:        input,
		transcript:   transcript,
		spinner:      spinner.New(),
		width:        80,
		height:       24,
		status:       "ready",
		model:        model,
		reasoning:    reasoning,
		approvalMode: string(agent.ApprovalAuto),
		historyPos:   -1,
		streamLine:   -1,
	}
	m.setPrompt()

	var entries []session.Entry
	if len(initial) > 0 {
		entries = initial[0]
	}
	m.lines = transcriptLines(entries, m.width)
	m.refreshTranscript(true)
	m.updateLayout()
	return m
}

func transcriptLines(entries []session.Entry, width int) []string {
	lines := []string{tuiMutedStyle.Render("escape personal agent · /help for commands")}
	for _, entry := range entries {
		if entry.Message == nil {
			switch entry.Type {
			case session.TypeSessionInfo:
				if name := strings.TrimSpace(entry.Name); name != "" {
					lines = append(lines, tuiMutedStyle.Render("session: "+name))
				}
			case session.TypeCompaction:
				lines = append(lines, tuiMutedStyle.Render("[compact] "+firstLine(entry.Summary)))
			case session.TypeBranchSummary:
				lines = append(lines, tuiMutedStyle.Render("[branch] "+firstLine(entry.Recap)))
			}
			continue
		}

		message := entry.Message
		for _, block := range message.Content {
			switch block.Type {
			case session.BlockText:
				switch message.Role {
				case session.RoleUser:
					lines = append(lines, tuiUserStyle.Render("> "+block.Text))
				case session.RoleAssistant:
					if strings.TrimSpace(block.Text) != "" {
						lines = appendMarkdown(lines, block.Text, width)
					}
				}
			case session.BlockToolCall:
				lines = append(lines, tuiToolStyle.Render("[tool] "+block.Name+" "+compactArgs(block.Arguments)))
			}
		}
		if message.Role == session.RoleToolResult {
			prefix := "[result] "
			if message.IsError {
				prefix = tuiErrorStyle.Render("[error] ")
			}
			lines = append(lines, prefix+truncateToolOutput(message.Text(false)))
		}
	}
	if len(lines) == 1 {
		lines = append(lines, tuiMutedStyle.Render("Ready."))
	}
	return lines
}

func renderMarkdown(text string, width int) string {
	text = agent.StripThinking(text)
	if strings.TrimSpace(text) == "" {
		return ""
	}
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(max(20, width)),
	)
	if err != nil {
		return text
	}
	rendered, err := renderer.Render(text)
	if err != nil {
		return text
	}
	return strings.TrimRight(rendered, "\n")
}

func appendMarkdown(lines []string, text string, width int) []string {
	rendered := renderMarkdown(text, width)
	if rendered == "" {
		return lines
	}
	return append(lines, strings.Split(rendered, "\n")...)
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return text[:i]
	}
	return text
}

func (m *tuiModel) Init() tea.Cmd {
	return tea.Batch(m.waitEvent(), textarea.Blink, m.spinner.Tick)
}

func (m *tuiModel) waitEvent() tea.Cmd {
	return func() tea.Msg {
		event, ok := <-m.events
		if !ok {
			return tuiClosedMsg{}
		}
		return tuiEventMsg{event: event}
	}
}

func (m *tuiModel) answer(id, text string) tea.Cmd {
	return func() tea.Msg {
		return tuiAnswerMsg{err: m.agent.AnswerQuestion(id, text)}
	}
}

func (m *tuiModel) answerApproval(id string, approved bool) tea.Cmd {
	return func() tea.Msg {
		return tuiApprovalMsg{err: m.agent.AnswerApproval(id, approved)}
	}
}

func (m *tuiModel) send(text string) tea.Cmd {
	return func() tea.Msg {
		_, err := m.agent.Send(text)
		return tuiSendMsg{err: err}
	}
}

func (m *tuiModel) diff(paths ...string) tea.Cmd {
	return func() tea.Msg {
		text, err := workspaceDiff(m.cwd, paths...)
		if err != nil {
			return tuiControlMsg{text: "diff error: " + err.Error()}
		}
		return tuiControlMsg{text: text}
	}
}

func (m *tuiModel) control(text string) tea.Cmd {
	return func() tea.Msg {
		var result string
		var err error
		switch text {
		case "/compact":
			var compacted *agent.CompactionResult
			compacted, err = m.agent.Compact(context.Background(), "manual CLI compaction")
			if err == nil {
				result = "compacted: " + compacted.FirstKeptEntryID
			}
		case "/snapcompact":
			var compacted *agent.CompactionResult
			compacted, err = m.agent.Snapcompact(context.Background())
			if err == nil {
				result = "snapcompact: " + compacted.FirstKeptEntryID
			}
		case "/recap":
			var recap string
			recap, err = m.agent.Recap(context.Background())
			if err == nil {
				result = recap
			}
		case "/state":
			state := m.agent.State()
			result = fmt.Sprintf("state: %s model=%s", state.State, state.Model)
		case "/login":
			result = "Run escape login from a terminal to choose a provider."
		default:
			err = fmt.Errorf("unknown command %s", text)
		}
		if err != nil {
			return tuiControlMsg{text: "error: " + err.Error()}
		}
		return tuiControlMsg{text: result}
	}
}

func (m *tuiModel) historyPrevious() {
	if len(m.history) == 0 {
		return
	}
	if m.historyPos == -1 {
		m.historyPos = len(m.history) - 1
	} else if m.historyPos > 0 {
		m.historyPos--
	}
	m.input.SetValue(m.history[m.historyPos])
	m.input.MoveToEnd()
	m.updateSuggestions()
}

func (m *tuiModel) historyNext() {
	if len(m.history) == 0 || m.historyPos == -1 {
		return
	}
	if m.historyPos < len(m.history)-1 {
		m.historyPos++
		m.input.SetValue(m.history[m.historyPos])
		m.input.MoveToEnd()
	} else {
		m.historyPos = -1
		m.input.Reset()
	}
	m.updateSuggestions()
}

func (m *tuiModel) updateSuggestions() {
	defer m.updateLayout()
	value := m.input.Value()
	if m.suggestionsInitialized && value == m.suggestionValue {
		return
	}
	m.suggestionsInitialized = true
	m.suggestionValue = value
	m.suggestions = nil
	m.commandPos = 0
	if value == "" || value[0] != '/' || strings.ContainsAny(value, " \t\n") {
		return
	}
	for _, command := range m.commands() {
		if strings.HasPrefix(command.name, value) {
			m.suggestions = append(m.suggestions, command)
		}
	}
}

func (m *tuiModel) listAllCommands() {
	commands := m.commands()
	lines := make([]string, 0, len(commands))
	for _, command := range commands {
		line := command.name
		if command.argumentHint != "" {
			line += " " + command.argumentHint
		}
		if command.description != "" {
			line += "  " + command.description
		}
		lines = append(lines, line)
	}
	m.appendLine("Commands:\n"+strings.Join(lines, "\n"), true)
}

func (m *tuiModel) commands() []tuiCommand {
	commands := append([]tuiCommand(nil), tuiCommands...)
	if m.resourceLoader == nil {
		return commands
	}
	for _, command := range m.resourceLoader.GetCommands() {
		name := command.Name
		if !strings.HasPrefix(name, "/") {
			name = "/" + name
		}
		if m.hasBuiltin(name) {
			continue
		}
		commands = append(commands, tuiCommand{name: name, description: command.Description, argumentHint: command.ArgumentHint})
	}
	return commands
}

func (m *tuiModel) hasBuiltin(name string) bool {
	for _, command := range tuiCommands {
		if command.name == name {
			return true
		}
	}
	return false
}

func expandResourceCommand(loader *resources.Loader, text string) (string, bool) {
	if loader == nil || !strings.HasPrefix(text, "/") {
		return "", false
	}
	parts := strings.Fields(text)
	if len(parts) == 0 || builtinCommand(parts[0]) {
		return "", false
	}
	name := strings.TrimPrefix(parts[0], "/")
	args := strings.TrimSpace(strings.TrimPrefix(text, parts[0]))
	if strings.HasPrefix(name, "skill:") {
		return loader.SkillCommand(strings.TrimPrefix(name, "skill:"), args)
	}
	return loader.ExpandTemplate(name, args)
}

func builtinCommand(name string) bool {
	for _, command := range tuiCommands {
		if command.name == name {
			return true
		}
	}
	return false
}

func (m *tuiModel) completeCommand() {
	if len(m.suggestions) == 0 {
		return
	}
	command := m.suggestions[m.commandPos%len(m.suggestions)]
	m.input.SetValue(command.name)
	m.input.MoveToEnd()
	m.commandPos = (m.commandPos + 1) % len(m.suggestions)
}

func (m *tuiModel) submit(text string) tea.Cmd {
	m.input.Reset()
	m.updateSuggestions()
	if text == "/exit" || text == "/quit" {
		return tea.Quit
	}
	if text == "" {
		return nil
	}
	if m.questionID != "" {
		id := m.questionID
		m.questionID = ""
		return m.answer(id, text)
	}
	if m.approvalID != "" {
		id := m.approvalID
		m.approvalID = ""
		approved := strings.EqualFold(text, "y") || strings.EqualFold(text, "yes")
		return m.answerApproval(id, approved)
	}
	if strings.HasPrefix(text, "/") {
		m.appendLine(tuiUserStyle.Render("> "+text), true)
		return m.runCommand(text)
	}

	m.history = append(m.history, text)
	if len(m.history) > 100 {
		m.history = m.history[len(m.history)-100:]
	}
	m.historyPos = -1
	m.appendLine(tuiUserStyle.Render("> "+text), true)
	if m.busy {
		if _, err := m.agent.QueueSteer(text); err != nil {
			m.appendLine(tuiErrorStyle.Render("queue error: "+err.Error()), true)
		} else {
			m.status = "queued steering"
		}
		return nil
	}
	m.setBusy(true)
	return m.send(text)
}

func (m *tuiModel) runCommand(text string) tea.Cmd {
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return nil
	}
	switch parts[0] {
	case "/stop":
		m.agent.Stop()
		m.status = "stopping"
		return nil
	case "/commands":
		m.listAllCommands()
		return nil
	case "/help":
		m.appendLine("/help /commands /diff /review /permissions [auto|ask] /rename <name> /recap /export <path> /clear /undo /fork <n> /forks /followup <text> /sessions [n] /new /login /model [id] /reasoning [level] /state /status /compact /snapcompact /stop /exit", true)
		return nil
	case "/forks":
		return m.listForkPoints()
	case "/fork":
		if len(parts) != 2 {
			m.appendLine("usage: /fork <user-message-number>", true)
			return nil
		}
		if m.busy {
			m.appendLine("stop the current turn before forking", true)
			return nil
		}
		index, err := strconv.Atoi(parts[1])
		if err != nil || index < 1 {
			m.appendLine("usage: /fork <user-message-number>", true)
			return nil
		}
		m.setBusy(true)
		return m.forkSession(index - 1)
	case "/undo":
		if m.busy {
			m.appendLine("stop the current turn before undoing", true)
			return nil
		}
		m.setBusy(true)
		return m.undoLastTurn()
	case "/clear":
		if m.busy {
			m.appendLine("stop the current turn before clearing the transcript", true)
			return nil
		}
		m.lines = transcriptLines(nil, m.width)
		m.refreshTranscript(true)
		m.updateLayout()
		return nil
	case "/export":
		if len(parts) != 2 {
			m.appendLine("usage: /export <path>", true)
			return nil
		}
		if m.busy {
			m.appendLine("stop the current turn before exporting", true)
			return nil
		}
		m.setBusy(true)
		return m.exportSession(parts[1])
	case "/rename":
		if len(parts) != 2 {
			m.appendLine("usage: /rename <name>", true)
			return nil
		}
		if m.busy {
			m.appendLine("stop the current turn before renaming the session", true)
			return nil
		}
		if err := m.agent.Rename(parts[1]); err != nil {
			m.appendLine(tuiErrorStyle.Render("rename error: "+err.Error()), true)
			return nil
		}
		m.appendLine("session name: "+parts[1], true)
		return nil
	case "/permissions":
		if len(parts) == 1 {
			m.appendLine("permissions: "+m.approvalMode, true)
			return nil
		}
		if len(parts) != 2 || (parts[1] != string(agent.ApprovalAuto) && parts[1] != string(agent.ApprovalAsk)) {
			m.appendLine("usage: /permissions [auto|ask]", true)
			return nil
		}
		if m.busy {
			m.appendLine("stop the current turn before changing permissions", true)
			return nil
		}
		if err := m.agent.SetApprovalMode(agent.ApprovalMode(parts[1])); err != nil {
			m.appendLine(tuiErrorStyle.Render("permissions error: "+err.Error()), true)
			return nil
		}
		m.approvalMode = parts[1]
		m.appendLine("permissions: "+m.approvalMode, true)
		return nil
	case "/review":
		if m.busy {
			m.appendLine("stop the current turn before starting a review", true)
			return nil
		}
		m.setBusy(true)
		return m.send(tuiReviewPromptFor(parts[1:]...))
	case "/new":
		if m.busy {
			m.appendLine("stop the current turn before starting a new session", true)
			return nil
		}
		m.setBusy(true)
		return m.newSession()
	case "/sessions":
		if len(parts) == 1 {
			return m.listSessions()
		}
		if len(parts) != 2 {
			m.appendLine("usage: /sessions [n]", true)
			return nil
		}
		index, err := strconv.Atoi(parts[1])
		if err != nil || index < 1 || index > len(m.sessionItems) {
			m.appendLine("usage: /sessions <number>", true)
			return nil
		}
		if m.busy {
			m.appendLine("stop the current turn before switching sessions", true)
			return nil
		}
		return m.switchSession(index - 1)
	case "/followup":
		followup := strings.TrimSpace(strings.TrimPrefix(text, "/followup"))
		if followup == "" {
			m.appendLine("usage: /followup <text>", true)
			return nil
		}
		if m.busy {
			if _, err := m.agent.FollowUp(followup); err != nil {
				m.appendLine(tuiErrorStyle.Render("follow-up error: "+err.Error()), true)
			} else {
				m.status = "queued follow-up"
			}
			return nil
		}
		m.setBusy(true)
		return m.send(followup)
	case "/model":
		if len(parts) == 1 {
			m.appendLine("model: "+m.model, true)
		} else if len(parts) == 2 {
			m.model = parts[1]
			m.agent.SetModel(m.model)
			m.appendLine("model: "+m.model, true)
		} else {
			m.appendLine("usage: /model <id>", true)
		}
		return nil
	case "/reasoning":
		if len(parts) == 1 {
			m.appendLine("reasoning: "+m.reasoning, true)
		} else if len(parts) == 2 && validReasoning(parts[1]) {
			m.reasoning = parts[1]
			m.agent.SetThinkingLevel(m.reasoning)
			m.appendLine("reasoning: "+m.reasoning, true)
		} else {
			m.appendLine("usage: /reasoning off|minimal|low|medium|high|xhigh", true)
		}
		return nil
	case "/diff":
		m.setBusy(true)
		return m.diff(parts[1:]...)
	case "/status":
		if m.busy {
			m.appendLine("stop the current turn before reading status", true)
			return nil
		}
		return m.sessionStatus()
	case "/login":
		return m.openLoginForm()
	case "/state", "/compact", "/snapcompact", "/recap":
		m.setBusy(true)
		return m.control(text)
	default:
		if expanded, ok := expandResourceCommand(m.resourceLoader, text); ok {
			m.setBusy(true)
			return m.send(expanded)
		}
		m.appendLine(tuiErrorStyle.Render("unknown command: "+parts[0]), true)
		return nil
	}
}

func (m *tuiModel) listSessions() tea.Cmd {
	root := m.sessionRoot
	if root == "" {
		root = session.DefaultRoot()
	}
	items, err := session.List(root)
	if err != nil {
		m.appendLine(tuiErrorStyle.Render("sessions error: "+err.Error()), true)
		return nil
	}
	filtered := items[:0]
	for _, item := range items {
		if m.cwd == "" || filepath.Clean(item.Cwd) == filepath.Clean(m.cwd) {
			filtered = append(filtered, item)
		}
	}
	m.sessionItems = filtered
	if len(filtered) == 0 {
		m.appendLine("No sessions found for this working directory.", true)
		return nil
	}
	var lines []string
	for i, item := range filtered {
		name := item.Name
		if name == "" {
			name = "(untitled)"
		}
		lines = append(lines, fmt.Sprintf("%d. %s  %s", i+1, name, item.Path))
	}
	m.appendLine("Sessions:\n"+strings.Join(lines, "\n")+"\nUse /sessions <number> to switch.", true)
	return nil
}

func (m *tuiModel) newSession() tea.Cmd {
	oldStore := m.store
	cwd := m.cwd
	return func() tea.Msg {
		root := m.sessionRoot
		if root == "" {
			root = session.DefaultRoot()
		}
		path, err := session.NewPath(root, cwd)
		if err != nil {
			return tuiSwitchSessionMsg{err: err}
		}
		next, err := session.Open(path, cwd)
		if err != nil {
			return tuiSwitchSessionMsg{err: err}
		}
		if err := m.agent.SwitchStore(next); err != nil {
			_ = next.Close()
			return tuiSwitchSessionMsg{err: err}
		}
		if oldStore != nil {
			_ = oldStore.Close()
		}
		return tuiSwitchSessionMsg{store: next, text: "started new session: " + path}
	}
}

func (m *tuiModel) switchSession(index int) tea.Cmd {
	item := m.sessionItems[index]
	oldStore := m.store
	return func() tea.Msg {
		next, err := session.Open(item.Path, item.Cwd)
		if err != nil {
			return tuiSwitchSessionMsg{err: err}
		}
		if err := m.agent.SwitchStore(next); err != nil {
			_ = next.Close()
			return tuiSwitchSessionMsg{err: err}
		}
		if oldStore != nil {
			_ = oldStore.Close()
		}
		return tuiSwitchSessionMsg{store: next, text: "switched to session: " + item.Path}
	}
}

func undoSessionPath(srcPath, cwd string, roots ...string) (string, error) {
	return session.UndoPath(srcPath, cwd, roots...)
}

func (m *tuiModel) listForkPoints() tea.Cmd {
	if m.busy {
		m.appendLine("stop the current turn before listing fork points", true)
		return nil
	}
	if m.store == nil {
		m.appendLine("fork points error: no active session", true)
		return nil
	}
	messages, err := session.GetForkMessages(m.store.Path())
	if err != nil {
		m.appendLine(tuiErrorStyle.Render("fork points error: "+err.Error()), true)
		return nil
	}
	if len(messages) == 0 {
		m.appendLine("No user-message fork points.", true)
		return nil
	}
	lines := make([]string, 0, len(messages))
	for i, message := range messages {
		text := strings.TrimSpace(strings.ReplaceAll(message.Text, "\n", " "))
		if len(text) > 100 {
			text = text[:100] + "…"
		}
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, text))
	}
	m.appendLine("Fork points:\n"+strings.Join(lines, "\n")+"\nUse /fork <number> to branch.", true)
	return nil
}

func forkSessionPath(srcPath, cwd string, index int, roots ...string) (string, error) {
	root := session.DefaultRoot()
	if len(roots) > 0 && roots[0] != "" {
		root = roots[0]
	}
	messages, err := session.GetForkMessages(srcPath)
	if err != nil {
		return "", err
	}
	if index < 0 || index >= len(messages) {
		return "", fmt.Errorf("user message %d is out of range", index+1)
	}
	newPath, _, err := session.ForkToRoot(srcPath, messages[index].EntryID, cwd, root)
	return newPath, err
}

func (m *tuiModel) forkSession(index int) tea.Cmd {
	srcPath := ""
	if m.store != nil {
		srcPath = m.store.Path()
	}
	oldStore := m.store
	cwd := m.cwd
	return func() tea.Msg {
		if srcPath == "" {
			return tuiSwitchSessionMsg{err: fmt.Errorf("no active session")}
		}
		newPath, err := forkSessionPath(srcPath, cwd, index, m.sessionRoot)
		if err != nil {
			return tuiSwitchSessionMsg{err: err}
		}
		next, err := session.Open(newPath, cwd)
		if err != nil {
			return tuiSwitchSessionMsg{err: err}
		}
		if err := m.agent.SwitchStore(next); err != nil {
			_ = next.Close()
			return tuiSwitchSessionMsg{err: err}
		}
		if oldStore != nil {
			_ = oldStore.Close()
		}
		return tuiSwitchSessionMsg{store: next, text: "forked from user message " + strconv.Itoa(index+1)}
	}
}

func (m *tuiModel) undoLastTurn() tea.Cmd {
	srcPath := ""
	if m.store != nil {
		srcPath = m.store.Path()
	}
	oldStore := m.store
	cwd := m.cwd
	return func() tea.Msg {
		if srcPath == "" {
			return tuiSwitchSessionMsg{err: fmt.Errorf("no active session")}
		}
		newPath, err := undoSessionPath(srcPath, cwd, m.sessionRoot)
		if err != nil {
			return tuiSwitchSessionMsg{err: err}
		}
		next, err := session.Open(newPath, cwd)
		if err != nil {
			return tuiSwitchSessionMsg{err: err}
		}
		if err := m.agent.SwitchStore(next); err != nil {
			_ = next.Close()
			return tuiSwitchSessionMsg{err: err}
		}
		if oldStore != nil {
			_ = oldStore.Close()
		}
		return tuiSwitchSessionMsg{store: next, text: "undid the latest user turn"}
	}
}

func (m *tuiModel) sessionStatus() tea.Cmd {
	path := ""
	if m.store != nil {
		path = m.store.Path()
	}
	return func() tea.Msg {
		if path == "" {
			return tuiControlMsg{text: "status error: no active session"}
		}
		stats, err := session.Stats(path, 0)
		if err != nil {
			return tuiControlMsg{text: "status error: " + err.Error()}
		}
		context := "unknown"
		if stats.ContextUsage != nil && stats.ContextUsage.Tokens != nil {
			context = fmt.Sprintf("%d/%d tokens", *stats.ContextUsage.Tokens, stats.ContextUsage.ContextWindow)
			if stats.ContextUsage.Percent != nil {
				context += fmt.Sprintf(" (%d%%)", *stats.ContextUsage.Percent)
			}
		}
		return tuiControlMsg{text: fmt.Sprintf("status: %d messages · %d tokens · $%.4f · context %s", stats.TotalMessages, stats.Tokens.Total, stats.Cost, context)}
	}
}

func (m *tuiModel) exportSession(path string) tea.Cmd {
	storePath := ""
	if m.store != nil {
		storePath = m.store.Path()
	}
	return func() tea.Msg {
		if storePath == "" {
			return tuiExportMsg{err: fmt.Errorf("no active session")}
		}
		var err error
		if strings.EqualFold(filepath.Ext(path), ".html") {
			err = session.ExportHTML(storePath, path)
		} else {
			err = session.ExportJSONL(storePath, path)
		}
		if err != nil {
			return tuiExportMsg{err: err}
		}
		return tuiExportMsg{text: "exported session: " + path}
	}
}

func (m *tuiModel) appendLine(line string, follow bool) {
	m.lines = append(m.lines, line)
	m.refreshTranscript(follow)
	m.updateLayout()
}

func (m *tuiModel) refreshTranscript(follow bool) {
	wasAtBottom := m.transcript.AtBottom()
	m.transcript.SetContent(strings.Join(m.lines, "\n"))
	if follow || wasAtBottom {
		m.transcript.GotoBottom()
	}
}

func (m *tuiModel) setPrompt() {
	prompt := tuiUserStyle.Render("> ")
	if m.busy {
		prompt = tuiBusyStyle.Render("… ")
	}
	m.input.SetPromptFunc(2, func(textarea.PromptInfo) string { return prompt })
	m.input.SetWidth(m.width)
}

func (m *tuiModel) setBusy(busy bool) {
	m.busy = busy
	if busy {
		m.status = "working"
	} else {
		m.status = "ready"
	}
	m.setPrompt()
	m.updateLayout()
}

func (m *tuiModel) updateLayout() {
	statusHeight := lipgloss.Height(m.statusView())
	inputHeight := m.input.Height()
	// Keep a fixed command dock so showing suggestions never moves the status
	// line or composer. The rendered view has two additional chrome rows: the
	// separator row and the line break before the status row.
	transcriptHeight := m.height - tuiCommandAreaHeight - statusHeight - inputHeight - 2
	m.transcript.SetWidth(m.width)
	m.transcript.SetHeight(max(1, transcriptHeight))
}

func (m *tuiModel) commandView() string {
	if len(m.suggestions) == 0 {
		return ""
	}
	visible := m.suggestions
	if len(visible) > 4 {
		visible = visible[:4]
	}
	lines := make([]string, 0, len(visible)+1)
	lines = append(lines, tuiMutedStyle.Render("commands · tab completes"))
	for i, command := range visible {
		line := command.name
		if command.argumentHint != "" {
			line += " " + command.argumentHint
		}
		if command.description != "" {
			line += "  " + command.description
		}
		if i == m.commandPos%len(m.suggestions) {
			line = tuiSelectedStyle.Render("› " + line)
		} else {
			line = "  " + line
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (m *tuiModel) statusView() string {
	status := m.status
	if m.busy {
		status = m.spinner.View() + " " + status
	}
	model := m.model
	if model == "" {
		model = "default"
	}
	reasoning := m.reasoning
	if reasoning == "" {
		reasoning = "default"
	}
	left := tuiStatusStyle.Render(status + "  " + model + "  " + reasoning)
	right := tuiMutedStyle.Render(fmt.Sprintf("%3.0f%%", m.transcript.ScrollPercent()*100))
	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.loginForm != nil {
		return m.updateLoginForm(msg)
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = max(20, msg.Width)
		m.height = max(8, msg.Height)
		m.input.SetWidth(m.width)
		m.updateLayout()
		return m, nil
	case cursor.BlinkMsg:
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tea.MouseMsg:
		var cmd tea.Cmd
		m.transcript, cmd = m.transcript.Update(msg)
		return m, cmd
	case tea.PasteMsg:
		m.input.InsertString(msg.String())
		m.updateSuggestions()
		return m, nil
	case tea.KeyPressMsg:
		key := msg.String()
		switch key {
		case "ctrl+c":
			if m.busy {
				m.agent.Stop()
				m.status = "stopping"
				return m, nil
			}
			return m, tea.Quit
		case "esc":
			if m.busy {
				m.agent.Stop()
				m.status = "stopping"
			}
			return m, nil
		case "ctrl+l":
			if m.busy {
				return m, nil
			}
			m.lines = transcriptLines(nil, m.width)
			m.refreshTranscript(true)
			m.updateLayout()
			return m, nil
		case "pgup":
			m.transcript.PageUp()
			return m, nil
		case "pgdown":
			m.transcript.PageDown()
			return m, nil
		case "up":
			if m.input.LineCount() == 1 {
				m.historyPrevious()
				return m, nil
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		case "down":
			if m.input.LineCount() == 1 {
				m.historyNext()
				return m, nil
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		case "tab":
			if len(m.suggestions) > 0 {
				m.completeCommand()
			}
			return m, nil
		case "enter":
			return m, m.submit(strings.TrimSpace(m.input.Value()))
		default:
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			m.updateSuggestions()
			return m, cmd
		}
	case tuiAnswerMsg:
		if msg.err != nil {
			m.appendLine(tuiErrorStyle.Render("error: "+msg.err.Error()), true)
		}
		return m, nil
	case tuiApprovalMsg:
		if msg.err != nil {
			m.appendLine(tuiErrorStyle.Render("approval error: "+msg.err.Error()), true)
		}
		return m, nil
	case tuiExportMsg:
		m.setBusy(false)
		if msg.err != nil {
			m.appendLine(tuiErrorStyle.Render("export error: "+msg.err.Error()), true)
		} else {
			m.appendLine(msg.text, true)
		}
		return m, nil
	case tuiSwitchSessionMsg:
		m.setBusy(false)
		if msg.err != nil {
			m.appendLine(tuiErrorStyle.Render("session switch error: "+msg.err.Error()), true)
			return m, nil
		}
		m.store = msg.store
		entries, _, _ := session.Tail(msg.store.Path(), 2<<20)
		m.lines = transcriptLines(entries, m.width)
		m.refreshTranscript(true)
		m.updateLayout()
		m.appendLine(msg.text, true)
		return m, nil
	case tuiSendMsg:
		if msg.err != nil {
			m.setBusy(false)
			m.status = "error"
			m.appendLine(tuiErrorStyle.Render("error: "+msg.err.Error()), true)
		}
		return m, nil
	case tuiControlMsg:
		m.setBusy(false)
		m.appendLine(msg.text, true)
		return m, nil
	case tuiLoginResultMsg:
		m.setBusy(false)
		if msg.err != nil {
			m.appendLine(tuiErrorStyle.Render("login error: "+msg.err.Error()), true)
			return m, m.waitEvent()
		}
		if err := m.applyTUIProvider(msg.provider); err != nil {
			m.appendLine(tuiErrorStyle.Render("login error: "+err.Error()), true)
			return m, m.waitEvent()
		}
		m.status = "ready — " + loginProviderLabel(msg.provider)
		m.appendLine(loginProviderLabel(msg.provider)+" is now the active provider", true)
		return m, m.waitEvent()
	case tuiEventMsg:
		event := msg.event
		switch event.Event {
		case agent.EventMessageStart:
			if event.Role == session.RoleAssistant {
				m.streamLine = len(m.lines)
				m.appendLine("", false)
			}
		case agent.EventMessageDelta:
			if m.streamLine < 0 || m.streamLine >= len(m.lines) {
				m.streamLine = len(m.lines)
				m.appendLine("", false)
			}
			atBottom := m.transcript.AtBottom()
			m.lines[m.streamLine] += event.Text
			m.refreshTranscript(atBottom)
		case agent.EventMessageEnd:
			if event.Role == session.RoleAssistant && m.streamLine >= 0 && m.streamLine < len(m.lines) {
				atBottom := m.transcript.AtBottom()
				rendered := renderMarkdown(m.lines[m.streamLine], m.width)
				replacement := strings.Split(rendered, "\n")
				if len(replacement) == 1 && replacement[0] == "" {
					replacement = nil
				}
				lines := make([]string, 0, len(m.lines)-1+len(replacement))
				lines = append(lines, m.lines[:m.streamLine]...)
				lines = append(lines, replacement...)
				lines = append(lines, m.lines[m.streamLine+1:]...)
				m.lines = lines
				m.refreshTranscript(atBottom)
				m.updateLayout()
			}
		case agent.EventToolCall:
			m.appendLine(tuiToolStyle.Render("[tool] "+event.Name+" "+compactArgs(event.Args)), false)
		case agent.EventToolResult:
			line := tuiMutedStyle.Render("[result] " + truncateToolOutput(event.Output))
			if event.Error {
				line = tuiErrorStyle.Render("[error] " + truncateToolOutput(event.Output))
			}
			m.appendLine(line, false)
		case agent.EventQuestionRequested:
			m.questionID = event.ToolCallID
			m.appendLine(tuiToolStyle.Render("[question] "+event.Question+"  answer below"), true)
		case agent.EventApprovalRequested:
			m.approvalID = event.ToolCallID
			m.appendLine(tuiToolStyle.Render(fmt.Sprintf("[approval] %s %s  (y/n)", event.Name, compactArgs(event.Args))), true)
		case agent.EventError:
			m.status = "error"
			m.appendLine(tuiErrorStyle.Render("error: "+event.Message), true)
		case agent.EventQueueUpdate:
			if len(event.Steering) > 0 {
				m.appendLine(tuiMutedStyle.Render("[queued] steering: "+strings.Join(event.Steering, " · ")), true)
			}
			if len(event.FollowUp) > 0 {
				m.appendLine(tuiMutedStyle.Render("[queued] follow-up: "+strings.Join(event.FollowUp, " · ")), true)
			}
		case agent.EventSettled:
			m.streamLine = -1
			m.setBusy(false)
			m.status = "ready — " + event.Reason
		}
		return m, m.waitEvent()
	case tuiClosedMsg:
		return m, tea.Quit
	}
	return m, nil
}

func (m *tuiModel) View() tea.View {
	if m.loginForm != nil {
		view := tea.NewView("\n" + m.loginForm.View())
		view.AltScreen = true
		view.MouseMode = tea.MouseModeCellMotion
		return view
	}
	// Keep the palette synchronized with the current composer value even when
	// an input event arrives through a path that did not call updateSuggestions.
	if !m.suggestionsInitialized || m.input.Value() != m.suggestionValue {
		m.updateSuggestions()
	}
	transcriptView := m.transcript.View()
	commandArea := m.commandView()
	if commandArea == "" {
		commandArea = tuiMutedStyle.Render("commands · type / to complete")
	}
	if height := lipgloss.Height(commandArea); height < tuiCommandAreaHeight {
		commandArea += strings.Repeat("\n", tuiCommandAreaHeight-height)
	}
	content := transcriptView + "\n" + commandArea
	content += "\n" + tuiSeparatorStyle.Render(strings.Repeat("─", max(1, m.width)))
	content += "\n" + m.statusView() + "\n" + m.input.View()

	view := tea.NewView(content)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	return view
}

func compactArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	data, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	if len(data) > 240 {
		return string(data[:240]) + "…"
	}
	return string(data)
}

func truncateToolOutput(output string) string {
	output = strings.TrimSpace(output)
	if len(output) > 500 {
		return output[:500] + "…"
	}
	return output
}

func validReasoning(level string) bool {
	switch level {
	case "off", "minimal", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

func runBubbleTea(ag *agent.Agent, input io.Reader, output io.Writer, cwd, sessionRoot, model, reasoning, approval string, set *settings.Settings, store *session.Store, initial []session.Entry) error {
	events := ag.Events()
	defer ag.Unsubscribe(events)
	m := newTUIModel(ag, events, model, reasoning, initial)
	m.cwd = cwd
	m.sessionRoot = sessionRoot
	m.approvalMode = approval
	m.resourceLoader = resources.NewQuiet(cwd, set)
	m.store = store
	defer func() { _ = m.store.Close() }()
	program := tea.NewProgram(m, tea.WithInput(input), tea.WithOutput(output))
	_, err := program.Run()
	return err
}
