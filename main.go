// Command escape is Escape's AI agent core in Go: a minimal agent loop
// (stream -> tools -> settle) with a REPL and stdio RPC modes for the GPUIX shell.
// Session files remain pi-compatible JSONL.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ekasc/escape-engine/internal/agent"
	"github.com/ekasc/escape-engine/internal/diagnostics"
	"github.com/ekasc/escape-engine/internal/memory"
	"github.com/ekasc/escape-engine/internal/provider"
	"github.com/ekasc/escape-engine/internal/resources"
	"github.com/ekasc/escape-engine/internal/rpc"
	"github.com/ekasc/escape-engine/internal/session"
	"github.com/ekasc/escape-engine/internal/settings"
	"github.com/ekasc/escape-engine/internal/tools"
)

const version = "0.1.0"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return cmdRepl(nil, stdin, stdout, stderr)
	}
	if strings.HasPrefix(args[0], "-") && args[0] != "--help" && args[0] != "-h" && args[0] != "--version" && args[0] != "-v" {
		return cmdRepl(args, stdin, stdout, stderr)
	}
	switch args[0] {
	case "repl":
		return cmdRepl(args[1:], stdin, stdout, stderr)
	case "ask":
		return cmdAsk(args[1:], stdin, stdout, stderr)
	case "serve":
		return cmdServe(args[1:], stdin, stdout, stderr)
	case "rpc":
		return cmdRPC(args[1:], stdin, stdout, stderr)
	case "diagnostics":
		return cmdDiagnostics(args[1:], stdout, stderr)
	case "index-sessions":
		return cmdIndexSessions(args, stdout, stderr)
	case "prune-sessions":
		return cmdPruneSessions(args, stdout, stderr)
	case "list-sessions":
		return cmdListSessions(args[1:], stdout, stderr)
	case "login":
		return cmdLogin(args[1:], stdin, stdout, stderr)
	case "bench-cache":
		return cmdCacheBenchmark(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "escape %s\n", version)
		return 0
	case "help", "--help", "-h":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "escape: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `escape — a personal agent runtime in Go

Usage:
  escape
      Start the interactive REPL.
  escape ask [--cwd DIR] [--model M] [--fake] [--session PATH] [--approval auto] "fix the failing test"
      Run one coding-agent turn and print the assistant response.
  escape repl [--cwd DIR] [--model M] [--fake] [--session PATH] [--resume] [--approval auto|ask]
      Interactive prompt -> stream -> result loop. Use --resume to load the
      newest session for --cwd. Commands: /commands, /diff, /review, /permissions [auto|ask], /rename <name>, /recap, /export <path>, /clear, /forks, /followup <text>, /sessions [n], /compact, /snapcompact, /stop, /state, /status, /exit.
  escape serve --session PATH --cwd DIR [--model M] [--fake]
      Stdio JSON-RPC control surface (requests on stdin, events+responses
      as JSON lines on stdout). This is what the Escape shell spawns.
  escape rpc [--session PATH] [--cwd DIR] [--model M] [--fake]
      Pi-compatible JSON-lines command/event protocol. Without --session,
      creates a new session in the configured session directory.
  escape list-sessions [--cwd DIR] [--json]
      Recent sessions, newest first.
  escape login
      Open the provider menu for OpenCode Go, OpenCode Zen, or Codex.
      Use --provider to skip the menu in scripts.
  escape bench-cache [--count N] [--prompt TEXT] [--provider NAME] [--model ID]
      Send identical requests and report provider-reported cache hit rate.
  escape version

Provider configuration (unless --fake):
  ESCAPE_PROVIDER   api, opencode-go, opencode-zen, or codex
  ESCAPE_BASE_URL   e.g. https://api.openai.com/v1 (or DeepSeek/Ollama/vLLM)
  ESCAPE_API_KEY    API key
  ESCAPE_MODEL      model id (default gpt-4o-mini)

Sessions are stored as pi-compatible JSONL under ESCAPE_SESSIONS_DIR
(default ~/.escape/sessions), readable by the Escape shell unchanged.
`)
}

// --- shared wiring ---

type providerConfig struct {
	fake          bool
	baseURL       string
	apiKey        string
	model         string
	maxTok        int
	thinkingLevel string
	providerName  string // "api" | "opencode-go" | "opencode-zen" | "codex" (empty = auto-detect)
}

// provider resolves the model backend. Each entry is a direct API client for
// a subscription/credential — never a wrapper around a CLI:
//
//   - "api":        OpenAI-compatible endpoint (ESCAPE_BASE_URL + API key)
//   - "opencode-go":  the OpenCode Go subscription using the OpenCode CLI key
//   - "opencode-zen": the OpenCode Zen subscription using the OpenCode CLI key
//   - "codex":        the Codex subscription via OpenAI OAuth
//
// With no explicit provider, the first available credential wins.
func (c providerConfig) provider() (provider.Provider, error) {
	if c.fake {
		return &provider.Fake{Handler: fakeHandler}, nil
	}
	name := c.providerName
	if name == "" {
		switch {
		case c.apiKey != "":
			name = "api"
		case provider.OpenCodeGoKey() != "":
			name = "opencode-go"
		case provider.OpenCodeZenKey() != "":
			name = "opencode-zen"
		case func() bool { _, err := provider.ChatGPTTokens(); return err == nil }():
			name = "codex"
		}
	}
	switch name {
	case "api":
		if c.apiKey == "" {
			return nil, errors.New("provider api: set ESCAPE_API_KEY (and ESCAPE_BASE_URL / ESCAPE_MODEL)")
		}
		p := provider.NewOpenAI(c.baseURL, c.apiKey, c.modelOr("gpt-4o-mini"))
		p.ReasoningEffort = c.thinkingLevel
		return p, nil
	case "opencode-go":
		key := provider.OpenCodeGoKey()
		if key == "" {
			return nil, errors.New("provider opencode-go: no key; run `escape login` or set OPENCODE_GO_API_KEY")
		}
		// The opencode.ai API expects the bare model id ("deepseek-v4-flash"),
		// never the picker's prefixed form ("opencode-go/deepseek-v4-flash").
		model := c.modelOr("minimax-m3")
		if i := strings.Index(model, "/"); i >= 0 && strings.HasPrefix(model, "opencode-go/") {
			model = model[i+1:]
		}
		p := provider.NewOpenCodeGo(key, model)
		p.ReasoningEffort = c.thinkingLevel
		return p, nil
	case "opencode-zen", "opencode":
		p := provider.NewOpenCodeZen(provider.OpenCodeZenKey(), c.modelOr(provider.OpenCodeZenDefaultModel))
		p.ReasoningEffort = c.thinkingLevel
		return p, nil
	case "codex", "openai", "chatgpt":
		tokens, err := provider.ChatGPTTokens()
		if err != nil {
			return nil, errors.New("provider codex: run `escape login` first")
		}
		p := provider.NewChatGPT(tokens, c.modelOr("gpt-5.6-luna"))
		p.ReasoningEffort = c.thinkingLevel
		return p, nil
	case "":
		return nil, provider.ErrNoSubscription
	default:
		return nil, fmt.Errorf("unknown provider %q (want api, opencode-go, opencode-zen, codex)", name)
	}
}

// modelOr returns c.model when explicitly configured, else the provider's
// default. ESCAPE_MODEL always wins over the default.
func (c providerConfig) modelOr(def string) string {
	if m := os.Getenv("ESCAPE_MODEL"); m != "" {
		return m
	}
	if c.model != "" {
		return c.model
	}
	return def
}

// fakeHandler is the offline demo model: it echoes the last user text and
// never calls tools. It also answers the naming and recap meta-calls
// deterministically so --fake sessions get a title and a recap.
func fakeHandler(ctx context.Context, req provider.Request) ([]provider.Event, error) {
	if agent.IsNamingRequest(req) {
		return []provider.Event{
			{Kind: provider.EventText, Text: fakeTitle(lastUserText(req))},
			{Kind: provider.EventDone, StopReason: "stop"},
		}, nil
	}
	if agent.IsRecapRequest(req) {
		return []provider.Event{
			{Kind: provider.EventText, Text: "Recap: the fake model summarized recent changes."},
			{Kind: provider.EventDone, StopReason: "stop"},
		}, nil
	}
	return []provider.Event{
		{Kind: provider.EventText, Text: "[fake model] " + lastUserText(req)},
		{Kind: provider.EventDone, StopReason: "stop"},
	}, nil
}

func lastUserText(req provider.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i].Text
		}
	}
	return ""
}

// fakeTitle derives a deterministic short title from the first user message.
func fakeTitle(text string) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return "Untitled session"
	}
	if len(words) > 4 {
		words = words[:4]
	}
	return strings.Join(words, " ")
}

func commonProviderFlags(fs *flag.FlagSet, c *providerConfig) {
	fs.BoolVar(&c.fake, "fake", false, "use the offline fake model (no network)")
	fs.StringVar(&c.providerName, "provider", os.Getenv("ESCAPE_PROVIDER"), "provider backend: api, opencode-go, opencode-zen, or codex")
	fs.StringVar(&c.baseURL, "base-url", envOr("ESCAPE_BASE_URL", "https://api.openai.com/v1"), "OpenAI-compatible base URL")
	fs.StringVar(&c.apiKey, "api-key", os.Getenv("ESCAPE_API_KEY"), "API key (or ESCAPE_API_KEY)")
	fs.StringVar(&c.model, "model", envOr("ESCAPE_MODEL", ""), "model id (or ESCAPE_MODEL)")
	fs.StringVar(&c.thinkingLevel, "thinking-level", envOr("ESCAPE_THINKING", ""), "reasoning level: minimal/low/medium/high/xhigh (or ESCAPE_THINKING)")
	fs.IntVar(&c.maxTok, "max-tokens", 8192, "max output tokens per model call")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// resolveSessionPath picks the session file: --session PATH if given, else a
// fresh pi-compatible path under the sessions root.
func resolveSessionPath(flagPath, cwd string) (string, error) {
	return resolveSessionPathInRoot(flagPath, cwd, session.DefaultRoot())
}

func resolveSessionPathInRoot(flagPath, cwd, root string) (string, error) {
	if flagPath != "" {
		abs, err := filepath.Abs(flagPath)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	return session.NewPath(root, cwd)
}

// resolveLiveSessionPath returns the project a shell should attach to. The shell
// launches `escape rpc` without a session, so an unconditional NewPath orphans
// a session on every launch, and a project accumulates one per open. Resolve the
// project's most recently touched session instead, matching `repl --resume`, and
// create one only when the project has no history yet.
func resolveLiveSessionPath(cwd, root string) (string, error) {
	info, err := latestSessionForCwd(root, cwd)
	if err != nil {
		return "", err
	}
	if info == nil {
		return session.NewPath(root, cwd)
	}
	return info.Path, nil
}

func configuredSessionRoot(value string) string { return settings.SessionRoot(value) }

// toolSetFor is the RPC server's tool builder. It is the same assembly a fresh
// start uses, so switching project cannot drift from starting in that directory.
func toolSetFor(cwd, sessionRoot string, set *settings.Settings) ([]tools.Tool, error) {
	return toolsForCwd(cwd, memory.Open(sessionRoot, cwd), sessionRoot, &session.Control{}, func() string { return "" }, set.DefaultTools)
}

func setProviderSessionID(p provider.Provider, id string) {
	if setter, ok := p.(interface{ SetSessionID(string) }); ok {
		setter.SetSessionID(id)
	}
}

// --- ask ---

func cmdAsk(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cwd string
	var sessionPath string
	var prompt string
	var approval string
	var cfg providerConfig
	fs.StringVar(&cwd, "cwd", "", "working directory (default: current)")
	fs.StringVar(&sessionPath, "session", "", "session file (default: new under sessions root)")
	fs.StringVar(&prompt, "prompt", "", "task to execute (or pass it as the first argument)")
	fs.StringVar(&approval, "approval", "", "tool approval mode: auto (ask is unavailable non-interactively)")
	commonProviderFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if prompt == "" && fs.NArg() > 0 {
		prompt = strings.Join(fs.Args(), " ")
	}
	if prompt == "" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintln(stderr, "escape ask: read prompt:", err)
			return 1
		}
		prompt = strings.TrimSpace(string(data))
	}
	if prompt == "" {
		fmt.Fprintln(stderr, "escape ask: provide a task or pipe one on stdin")
		return 2
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	set, err := settings.Load(cwd)
	if err != nil {
		fmt.Fprintln(stderr, "escape ask:", err)
		return 1
	}
	if cfg.providerName == "" {
		cfg.providerName = set.DefaultProvider
	}
	if cfg.model == "" {
		cfg.model = set.DefaultModel
	}
	if cfg.thinkingLevel == "" {
		cfg.thinkingLevel = set.DefaultThinkingLevel
	}
	if approval == "" {
		approval = set.ApprovalMode
	}
	if approval == "" {
		approval = string(agent.ApprovalAuto)
	}
	if approval != string(agent.ApprovalAuto) {
		if approval == string(agent.ApprovalAsk) {
			fmt.Fprintln(stderr, "escape ask: approval ask requires the interactive repl")
		} else {
			fmt.Fprintln(stderr, "escape ask: --approval must be auto")
		}
		return 2
	}
	sessionRoot := configuredSessionRoot(set.SessionDir)

	p, err := cfg.provider()
	if err != nil {
		fmt.Fprintln(stderr, "escape:", err)
		return 1
	}
	path, err := resolveSessionPathInRoot(sessionPath, cwd, sessionRoot)
	if err != nil {
		fmt.Fprintln(stderr, "escape:", err)
		return 1
	}
	store, err := session.Open(path, cwd)
	if err != nil {
		fmt.Fprintln(stderr, "escape:", err)
		return 1
	}
	defer store.Close()
	setProviderSessionID(p, store.ID())
	ctrl := &session.Control{}
	currentPath := ctrl.Current
	toolSet, err := toolsForCwd(cwd, memory.Open(sessionRoot, cwd), sessionRoot, ctrl, currentPath, set.DefaultTools)
	if err != nil {
		_ = store.Close()
		fmt.Fprintln(stderr, "escape ask:", err)
		return 1
	}

	ag := agent.New(agent.Options{
		Store:    store,
		Provider: p,
		Tools:    toolSet,
		Cwd:      cwd,
		Model:    cfg.model,
		Settings: set,
	})

	events := ag.Events()
	defer ag.Unsubscribe(events)
	settled := make(chan string, 1)
	go func() {
		for ev := range events {
			switch ev.Event {
			case agent.EventMessageDelta:
				fmt.Fprint(stdout, ev.Text)
			case agent.EventToolCall:
				fmt.Fprintf(stderr, "⚙ %s\n", ev.Name)
			case agent.EventToolResult:
				fmt.Fprintln(stderr, strings.TrimSpace(ev.Output))
			case agent.EventError:
				fmt.Fprintln(stderr, "error:", ev.Message)
			case agent.EventSettled:
				settled <- ev.Reason
				return
			}
		}
	}()

	if _, err := ag.Send(prompt); err != nil {
		fmt.Fprintln(stderr, "escape ask:", err)
		return 1
	}
	reason := <-settled
	fmt.Fprintln(stdout)
	if reason != agent.ReasonDone {
		return 1
	}
	return 0
}

// --- repl ---

func interactiveInput(input io.Reader) bool {
	file, ok := input.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func cmdRepl(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cwd string
	var sessionPath string
	var resume bool
	var approval string
	var cfg providerConfig
	fs.StringVar(&cwd, "cwd", "", "working directory (default: current)")
	fs.StringVar(&sessionPath, "session", "", "session file (default: new under sessions root)")
	fs.BoolVar(&resume, "resume", false, "resume the newest session for --cwd")
	fs.StringVar(&approval, "approval", "", "tool approval mode: auto or ask (default: settings or auto)")
	commonProviderFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	interactive := interactiveInput(stdin)
	set, err := settings.Load(cwd)
	if err != nil {
		fmt.Fprintln(stderr, "escape repl:", err)
		return 1
	}
	if approval == "" {
		approval = set.ApprovalMode
	}
	if approval == "" {
		approval = string(agent.ApprovalAuto)
	}
	sessionRoot := configuredSessionRoot(set.SessionDir)
	if cfg.providerName == "" {
		cfg.providerName = set.DefaultProvider
	}
	if cfg.model == "" {
		cfg.model = set.DefaultModel
	}
	if cfg.thinkingLevel == "" {
		cfg.thinkingLevel = set.DefaultThinkingLevel
	}
	if approval != string(agent.ApprovalAuto) && approval != string(agent.ApprovalAsk) {
		fmt.Fprintln(stderr, "escape repl: --approval must be auto or ask")
		return 2
	}
	if approval == string(agent.ApprovalAsk) && !interactive {
		fmt.Fprintln(stderr, "escape repl: --approval ask requires an interactive terminal")
		return 2
	}
	if resume && sessionPath != "" {
		fmt.Fprintln(stderr, "escape repl: use only one of --session or --resume")
		return 2
	}
	if resume {
		info, err := latestSessionForCwd(sessionRoot, cwd)
		if err != nil {
			fmt.Fprintln(stderr, "escape repl:", err)
			return 1
		}
		if info == nil {
			fmt.Fprintf(stderr, "escape repl: no session found for %s\n", cwd)
			return 1
		}
		sessionPath = info.Path
	}

	p, err := cfg.provider()
	if err != nil {
		fmt.Fprintln(stderr, "escape:", err)
		return 1
	}
	path, err := resolveSessionPathInRoot(sessionPath, cwd, sessionRoot)
	if err != nil {
		fmt.Fprintln(stderr, "escape:", err)
		return 1
	}
	store, err := session.Open(path, cwd)
	if err != nil {
		fmt.Fprintln(stderr, "escape:", err)
		return 1
	}
	setProviderSessionID(p, store.ID())
	ctrl := &session.Control{}
	currentPath := ctrl.Current
	toolSet, err := toolsForCwd(cwd, memory.Open(sessionRoot, cwd), sessionRoot, ctrl, currentPath, set.DefaultTools)
	if err != nil {
		_ = store.Close()
		fmt.Fprintln(stderr, "escape repl:", err)
		return 1
	}

	ag := agent.New(agent.Options{
		Store:                 store,
		Provider:              p,
		Tools:                 toolSet,
		Cwd:                   cwd,
		Model:                 cfg.model,
		Settings:              set,
		ApprovalMode:          agent.ApprovalMode(approval),
		QuietResourceWarnings: interactive,
	})

	if !interactive {
		defer store.Close()
		return replLoop(ag, stdin, stdout, stderr, cwd, path, sessionRoot, resources.New(cwd, set))
	}
	entries, _, _ := session.Tail(path, 2<<20)
	if err := runBubbleTea(ag, stdin, stdout, cwd, sessionRoot, cfg.model, cfg.thinkingLevel, approval, set, store, entries); err != nil {
		fmt.Fprintln(stderr, "escape repl:", err)
		return 1
	}
	return 0
}

// toolsForCwd builds the tool set for a project. mem is the project's durable
// memory store and ctrl carry session state between this tool set and the RPC
// server that owns the agent; both are always built, even when the caller
// filters tools by name, so a session that opts into "memory" or "sessions" can
// find its own.
func toolsForCwd(cwd string, mem *memory.Store, sessionRoot string, ctrl *session.Control, current func() string, names []string) ([]tools.Tool, error) {
	all := tools.Default(tools.Deps{
		Cwd:            cwd,
		Memory:         mem,
		SessionRoot:    sessionRoot,
		CurrentSession: current,
		Control:        ctrl,
	})
	if len(names) == 0 {
		return all, nil
	}
	byName := make(map[string]tools.Tool, len(all))
	for _, tool := range all {
		byName[tool.Name] = tool
	}
	selected := make([]tools.Tool, 0, len(names))
	for _, name := range names {
		tool, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("unknown default tool %q", name)
		}
		selected = append(selected, tool)
	}
	return selected, nil
}

func workspaceDiff(cwd string, paths ...string) (string, error) {
	return session.WorkspaceDiff(cwd, paths...)
}

func latestSessionForCwd(root, cwd string) (*session.Info, error) {
	lookup, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	// ListForCwd filters while probing headers, so resolving the live session
	// does not read every session in the store on engine startup.
	infos, err := session.ListForCwd(root, lookup)
	if err != nil {
		return nil, err
	}
	if len(infos) == 0 {
		return nil, nil
	}
	return &infos[0], nil
}

func replLoop(ag *agent.Agent, stdin io.Reader, stdout, stderr io.Writer, cwd, sessionPath, sessionRoot string, loader *resources.Loader) int {
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)

	fmt.Fprint(stdout, "> ")
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
		case line == "/exit" || line == "/quit":
			return 0
		case line == "/stop":
			ag.Stop()
			fmt.Fprintln(stdout, "[stopping…]")
		case line == "/model":
			fmt.Fprintf(stdout, "[model: %s]\n", ag.State().Model)
		case strings.HasPrefix(line, "/model "):
			model := strings.TrimSpace(strings.TrimPrefix(line, "/model "))
			if model == "" {
				fmt.Fprintln(stdout, "usage: /model <id>")
			} else {
				ag.SetModel(model)
				fmt.Fprintf(stdout, "[model: %s]\n", model)
			}
		case line == "/reasoning":
			fmt.Fprintln(stdout, "[reasoning: use /reasoning <level>]")
		case strings.HasPrefix(line, "/reasoning "):
			level := strings.TrimSpace(strings.TrimPrefix(line, "/reasoning "))
			if !validReasoning(level) {
				fmt.Fprintln(stdout, "usage: /reasoning off|minimal|low|medium|high|xhigh")
			} else {
				ag.SetThinkingLevel(level)
				fmt.Fprintf(stdout, "[reasoning: %s]\n", level)
			}
		case line == "/login":
			_ = cmdLogin(nil, stdin, stdout, stderr)
		case line == "/state":
			st := ag.State()
			fmt.Fprintf(stdout, "[state: %s turn=%s model=%s settle=%s]\n", st.State, st.TurnID, st.Model, st.SettleReason)
		case line == "/commands":
			for _, command := range tuiCommands {
				fmt.Fprintf(stdout, "%s  %s\n", command.name, command.description)
			}
			if loader != nil {
				for _, command := range loader.GetCommands() {
					line := command.Name
					if !strings.HasPrefix(line, "/") {
						line = "/" + line
					}
					if command.ArgumentHint != "" {
						line += " " + command.ArgumentHint
					}
					if command.Description != "" {
						line += "  " + command.Description
					}
					fmt.Fprintln(stdout, line)
				}
			}
		case line == "/status":
			stats, err := session.Stats(sessionPath, 0)
			if err != nil {
				fmt.Fprintln(stdout, "[status error]", err)
				break
			}
			context := "unknown"
			if stats.ContextUsage != nil && stats.ContextUsage.Tokens != nil {
				context = fmt.Sprintf("%d/%d tokens", *stats.ContextUsage.Tokens, stats.ContextUsage.ContextWindow)
				if stats.ContextUsage.Percent != nil {
					context += fmt.Sprintf(" (%d%%)", *stats.ContextUsage.Percent)
				}
			}
			fmt.Fprintf(stdout, "status: %d messages · %d tokens · $%.4f · context %s\n", stats.TotalMessages, stats.Tokens.Total, stats.Cost, context)
		case line == "/forks":
			messages, err := session.GetForkMessages(sessionPath)
			if err != nil {
				fmt.Fprintln(stdout, "[forks error]", err)
				break
			}
			if len(messages) == 0 {
				fmt.Fprintln(stdout, "No user-message fork points.")
				break
			}
			for i, message := range messages {
				text := strings.TrimSpace(strings.ReplaceAll(message.Text, "\n", " "))
				if len(text) > 100 {
					text = text[:100] + "…"
				}
				fmt.Fprintf(stdout, "%d. %s\n", i+1, text)
			}
		case line == "/diff" || strings.HasPrefix(line, "/diff "):
			parts := strings.Fields(line)
			var paths []string
			if len(parts) > 1 {
				paths = parts[1:]
			}
			text, err := workspaceDiff(cwd, paths...)
			if err != nil {
				fmt.Fprintln(stdout, "[diff error]", err)
			} else {
				fmt.Fprintln(stdout, text)
			}
		case line == "/review" || strings.HasPrefix(line, "/review "):
			parts := strings.Fields(line)
			var paths []string
			if len(parts) > 1 {
				paths = parts[1:]
			}
			runTurnInteractive(ag, tuiReviewPromptFor(paths...), stdout)
		case line == "/rename" || strings.HasPrefix(line, "/rename "):
			parts := strings.Fields(line)
			if len(parts) == 1 {
				fmt.Fprintln(stdout, "usage: /rename <name>")
			} else if err := ag.Rename(strings.Join(parts[1:], " ")); err != nil {
				fmt.Fprintln(stdout, "[rename error]", err)
			} else {
				fmt.Fprintf(stdout, "session name: %s\n", strings.Join(parts[1:], " "))
			}
		case line == "/recap":
			recap, err := ag.Recap(context.Background())
			if err != nil {
				fmt.Fprintln(stdout, "[recap error]", err)
			} else {
				fmt.Fprintln(stdout, recap)
			}
		case line == "/clear":
			fmt.Fprint(stdout, "\033[2J\033[H")
		case line == "/export" || strings.HasPrefix(line, "/export "):
			parts := strings.Fields(line)
			if len(parts) != 2 {
				fmt.Fprintln(stdout, "usage: /export <path>")
				break
			}
			var err error
			if strings.EqualFold(filepath.Ext(parts[1]), ".html") {
				err = session.ExportHTML(sessionPath, parts[1])
			} else {
				err = session.ExportJSONL(sessionPath, parts[1])
			}
			if err != nil {
				fmt.Fprintln(stdout, "[export error]", err)
			} else {
				fmt.Fprintln(stdout, "exported session:", parts[1])
			}
		case line == "/permissions" || strings.HasPrefix(line, "/permissions "):
			parts := strings.Fields(line)
			if len(parts) == 1 {
				state := ag.State()
				fmt.Fprintf(stdout, "permissions: %s\n", state.ApprovalMode)
			} else if len(parts) == 2 {
				if err := ag.SetApprovalMode(agent.ApprovalMode(parts[1])); err != nil {
					fmt.Fprintln(stdout, "[permissions error]", err)
				} else {
					fmt.Fprintf(stdout, "permissions: %s\n", parts[1])
				}
			} else {
				fmt.Fprintln(stdout, "usage: /permissions [auto|ask]")
			}
		case line == "/sessions":
			infos, err := session.List(sessionRoot)
			if err != nil {
				fmt.Fprintln(stdout, "[sessions error]", err)
				break
			}
			count := 0
			for _, info := range infos {
				if cwd != "" && filepath.Clean(info.Cwd) != filepath.Clean(cwd) {
					continue
				}
				count++
				name := info.Name
				if name == "" {
					name = "(untitled)"
				}
				fmt.Fprintf(stdout, "%d. %s  %s\n", count, name, info.Path)
			}
			if count == 0 {
				fmt.Fprintln(stdout, "No sessions found for this working directory.")
			}
		case strings.HasPrefix(line, "/followup"):
			followup := strings.TrimSpace(strings.TrimPrefix(line, "/followup"))
			if followup == "" {
				fmt.Fprintln(stdout, "usage: /followup <text>")
			} else {
				_, _ = ag.FollowUp(followup)
			}
		case line == "/compact":
			result, err := ag.Compact(context.Background(), "manual CLI compaction")
			if err != nil {
				fmt.Fprintf(stdout, "[compact error] %v\n", err)
			} else {
				fmt.Fprintf(stdout, "[compacted: %s -> %s]\n", result.FirstKeptEntryID, result.Summary)
			}
		case line == "/snapcompact":
			result, err := ag.Snapcompact(context.Background())
			if err != nil {
				fmt.Fprintf(stdout, "[snapcompact error] %v\n", err)
			} else {
				fmt.Fprintf(stdout, "[snapcompact: %s -> %s]\n", result.FirstKeptEntryID, result.Summary)
			}
		case line == "/help":
			fmt.Fprintln(stdout, "/commands /diff /review /permissions [auto|ask] /rename <name> /recap /export <path> /clear /forks /followup <text> /sessions /login /model [id] /reasoning [level] /stop /state /status /compact /snapcompact /exit")
		default:
			if expanded, ok := expandResourceCommand(loader, line); ok {
				runTurnInteractive(ag, expanded, stdout)
			} else {
				runTurnInteractive(ag, line, stdout)
			}
		}
		fmt.Fprint(stdout, "> ")
	}
	return 0
}

func runTurnInteractive(ag *agent.Agent, text string, stdout io.Writer) {
	ch := ag.Events()
	defer ag.Unsubscribe(ch)

	settled := make(chan string, 1)
	go func() {
		for ev := range ch {
			switch ev.Event {
			case agent.EventMessageDelta:
				fmt.Fprint(stdout, ev.Text)
			case agent.EventToolCall:
				args, _ := json.Marshal(ev.Args)
				fmt.Fprintf(stdout, "\n⚙ %s %s\n", ev.Name, args)
			case agent.EventToolResult:
				out := ev.Output
				if len(out) > 2000 {
					out = out[:2000] + "…"
				}
				fmt.Fprintf(stdout, "%s\n", out)
			case agent.EventError:
				fmt.Fprintf(stdout, "[error] %s\n", ev.Message)
			case agent.EventSettled:
				settled <- ev.Reason
				return
			}
		}
	}()

	if _, err := ag.Send(text); err != nil {
		fmt.Fprintf(stdout, "[error] %v\n", err)
		return
	}
	<-settled
	fmt.Fprintf(stdout, "\n[settled]\n")
}

// --- serve ---

func cmdServe(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cwd, sessionPath string
	var cfg providerConfig
	fs.StringVar(&cwd, "cwd", "", "working directory")
	fs.StringVar(&sessionPath, "session", "", "session file (required)")
	commonProviderFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if sessionPath == "" {
		fmt.Fprintln(stderr, "escape serve: --session PATH is required")
		return 2
	}
	set, err := settings.Load(cwd)
	if err != nil {
		fmt.Fprintln(stderr, "escape serve:", err)
		return 1
	}
	if cfg.providerName == "" {
		cfg.providerName = set.DefaultProvider
	}
	if cfg.model == "" {
		cfg.model = set.DefaultModel
	}
	if cfg.thinkingLevel == "" {
		cfg.thinkingLevel = set.DefaultThinkingLevel
	}

	p, err := cfg.provider()
	if err != nil {
		fmt.Fprintln(stderr, "escape:", err)
		return 1
	}
	path, err := filepath.Abs(sessionPath)
	if err != nil {
		fmt.Fprintln(stderr, "escape:", err)
		return 1
	}
	store, err := session.Open(path, cwd)
	if err != nil {
		fmt.Fprintln(stderr, "escape:", err)
		return 1
	}
	defer store.Close()
	setProviderSessionID(p, store.ID())

	// `set` is already loaded for this cwd; reuse its session dir.
	serveRoot := configuredSessionRoot(set.SessionDir)
	ctrl := &session.Control{}
	toolSet, err := toolsForCwd(cwd, memory.Open(serveRoot, cwd), serveRoot, ctrl, ctrl.Current, set.DefaultTools)
	if err != nil {
		fmt.Fprintln(stderr, "escape serve:", err)
		return 1
	}
	ag := agent.New(agent.Options{
		Store:    store,
		Provider: p,
		Tools:    toolSet,
		Cwd:      cwd,
		Model:    cfg.model,
		Settings: set,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := rpc.NewServer(ag, store, configuredSessionRoot(set.SessionDir), cwd, stdout)
	events := ag.Events()
	defer ag.Unsubscribe(events)
	go srv.Relay(ctx, events)

	// Serve until stdin closes (the parent's lifetime) or we're signaled.
	err = srv.Serve(ctx, stdin)
	if err != nil {
		fmt.Fprintln(stderr, "escape serve:", err)
		return 1
	}
	// Give the stopped turn a moment to emit its final events before exit.
	settleDone := make(chan struct{})
	go func() {
		ag.Wait()
		close(settleDone)
	}()
	select {
	case <-settleDone:
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
	}
	// Wait() signals when the turn settles, but the Relay goroutine may still
	// hold the agent_settled event in its channel buffer; a short drain lets
	// it reach stdout before the process exits.
	time.Sleep(100 * time.Millisecond)
	return 0
}

// --- rpc (pi command/event protocol) ---

// cmdRPC runs the pi-compatible command/event JSON-lines RPC protocol:
// stdin requests {type:"<command>",...}, stdout responses + events.
func cmdRPC(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rpc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cwd, sessionPath string
	var cfg providerConfig
	fs.StringVar(&cwd, "cwd", "", "working directory")
	fs.StringVar(&sessionPath, "session", "", "session file (default: new configured session)")
	commonProviderFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	set, err := settings.Load(cwd)
	if err != nil {
		fmt.Fprintln(stderr, "escape rpc:", err)
		return 1
	}
	if cfg.providerName == "" {
		cfg.providerName = set.DefaultProvider
	}
	if cfg.model == "" {
		cfg.model = set.DefaultModel
	}
	if cfg.thinkingLevel == "" {
		cfg.thinkingLevel = set.DefaultThinkingLevel
	}
	p, err := cfg.provider()
	if err != nil {
		fmt.Fprintln(stderr, "escape:", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sessionRoot := configuredSessionRoot(set.SessionDir)
	if sessionPath == "" {
		sessionPath, err = resolveLiveSessionPath(cwd, sessionRoot)
		if err != nil {
			fmt.Fprintln(stderr, "escape rpc:", err)
			return 1
		}
	}
	ctrl := &session.Control{}
	currentPath := ctrl.Current
	toolSet, err := toolsForCwd(cwd, memory.Open(sessionRoot, cwd), sessionRoot, ctrl, currentPath, set.DefaultTools)
	if err != nil {
		fmt.Fprintln(stderr, "escape rpc:", err)
		return 1
	}
	srv, err := rpc.NewPiServer(p, toolSet, set, cwd, sessionRoot, sessionPath, ctrl, stdout, toolSetFor)
	if err != nil {
		fmt.Fprintln(stderr, "escape rpc:", err)
		return 1
	}
	if err := srv.Serve(ctx, stdin); err != nil {
		fmt.Fprintln(stderr, "escape rpc:", err)
		return 1
	}
	return 0
}

// --- diagnostics ---

// cmdDiagnostics runs one real turn and reports where the time went.
//
// Every figure here is measured on a live provider request, not inferred. The
// phases are separated because they fail differently: reading history is disk
// and grows with the transcript, building the request is CPU and grows with the
// conversation, and the provider wait is the network and the model. Only the
// total tells you which one to go and fix.
func cmdDiagnostics(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("diagnostics", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cwd, sessionPath, prompt string
	var cfg providerConfig
	fs.StringVar(&cwd, "cwd", "", "working directory (default: current)")
	fs.StringVar(&sessionPath, "session", "", "session file to measure against (default: new)")
	fs.StringVar(&prompt, "prompt", "", "message to send (default: a short timing probe)")
	commonProviderFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if prompt == "" {
		prompt = "Reply with exactly the word: ready"
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	set, err := settings.Load(cwd)
	if err != nil {
		fmt.Fprintln(stderr, "escape diagnostics:", err)
		return 1
	}
	if cfg.providerName == "" {
		cfg.providerName = set.DefaultProvider
	}
	if cfg.model == "" {
		cfg.model = set.DefaultModel
	}
	if cfg.thinkingLevel == "" {
		cfg.thinkingLevel = set.DefaultThinkingLevel
	}
	// The real tool set is kept deliberately: its schemas are part of every
	// request, so dropping them would measure something nobody actually sends.
	toolSet, err := toolsForCwd(cwd, memory.Open(configuredSessionRoot(set.SessionDir), cwd),
		configuredSessionRoot(set.SessionDir), &session.Control{}, func() string { return "" }, nil)
	if err != nil {
		toolSet = nil
	}

	p, err := cfg.provider()
	if err != nil {
		fmt.Fprintln(stderr, "escape diagnostics:", err)
		return 1
	}
	root := configuredSessionRoot(set.SessionDir)
	path, err := resolveSessionPathInRoot(sessionPath, cwd, root)
	if err != nil {
		fmt.Fprintln(stderr, "escape diagnostics:", err)
		return 1
	}
	store, err := session.Open(path, cwd)
	if err != nil {
		fmt.Fprintln(stderr, "escape diagnostics:", err)
		return 1
	}
	defer store.Close()
	setProviderSessionID(p, store.ID())

	ag := agent.New(agent.Options{Store: store, Provider: p, Tools: toolSet, Cwd: cwd, Model: cfg.model, Settings: set})
	events := ag.Events()
	defer ag.Unsubscribe(events)
	settled := make(chan string, 1)
	go func() {
		for ev := range events {
			switch ev.Event {
			case agent.EventError:
				fmt.Fprintln(stderr, "error:", ev.Message)
			case agent.EventSettled:
				settled <- ev.Reason
				return
			}
		}
	}()

	if _, err := ag.Send(prompt); err != nil {
		fmt.Fprintln(stderr, "escape diagnostics:", err)
		return 1
	}
	<-settled

	snap := ag.Diagnostics().Snapshot()
	snap.Model = ag.Model()
	snap.Provider = cfg.providerName
	snap.Thinking = cfg.thinkingLevel
	snap.SessionPath = store.Path()
	snap.SessionEntries = ag.HistoryLen()
	snap.Tools = ag.ToolCount()
	if st, statErr := os.Stat(store.Path()); statErr == nil {
		snap.SessionSize = st.Size()
	}
	if info, statErr := os.Stat(store.Path()); statErr == nil {
		snap.SessionSize = info.Size()
	}
	diagnostics.WriteReport(stdout, snap)
	return 0
}

// --- index-sessions ---

// cmdIndexSessions rebuilds the session index from the sessions on disk.
//
// The index is derived and self-correcting, so this is never required for
// correctness. It exists to compact an index that has grown after sessions were
// removed, because entries for files that no longer exist are only dropped by
// a rebuild.
func cmdIndexSessions(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("index-sessions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root := configuredSessionRoot("")
	n, err := session.RefreshIndex(root)
	if err != nil {
		fmt.Fprintln(stderr, "escape:", err)
		return 1
	}
	size := 0
	if info, statErr := os.Stat(session.IndexPath(root)); statErr == nil {
		size = int(info.Size())
	}
	fmt.Fprintf(stdout, "indexed %d sessions, %s is %d KB\n", n, session.IndexPath(root), size/1024)
	return 0
}

// --- prune-sessions ---

// cmdPruneSessions removes project directories that hold no sessions.
//
// The store keeps a directory per project ever opened, and every listing walks
// all of them, so empty ones are pure latency. A directory is only removed when
// it holds no session file and no other file, and the removal re-checks, so a
// session written between the scan and the removal is never lost. Without
// --apply it only reports.
func cmdPruneSessions(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("prune-sessions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	apply := fs.Bool("apply", false, "remove the empty directories")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root := configuredSessionRoot("")
	found, err := session.PruneEmptyProjectDirs(root)
	if err != nil {
		fmt.Fprintln(stderr, "escape:", err)
		return 1
	}
	if !*apply {
		fmt.Fprintln(stdout, session.DescribePrune(found))
		if len(found.Empty) > 0 {
			fmt.Fprintln(stdout, "re-run with --apply to remove them")
		}
		return 0
	}
	removed, err := session.PruneEmptyProjectDirsApply(root)
	if err != nil {
		fmt.Fprintln(stderr, "escape:", err)
		return 1
	}
	fmt.Fprintf(stdout, "removed %d of %d empty project directories\n", removed, len(found.Empty))
	return 0
}

// --- list-sessions ---

func cmdListSessions(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list-sessions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cwd string
	var asJSON bool
	fs.StringVar(&cwd, "cwd", "", "only sessions for this directory")
	fs.BoolVar(&asJSON, "json", false, "emit JSON lines")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	infos, err := session.List(session.DefaultRoot())
	if err != nil {
		fmt.Fprintln(stderr, "escape:", err)
		return 1
	}
	if cwd != "" {
		abs, _ := filepath.Abs(cwd)
		filtered := infos[:0]
		for _, i := range infos {
			if i.Cwd == abs {
				filtered = append(filtered, i)
			}
		}
		infos = filtered
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		for _, i := range infos {
			_ = enc.Encode(i)
		}
		return 0
	}
	if len(infos) == 0 {
		fmt.Fprintln(stdout, "(no sessions)")
		return 0
	}
	for _, i := range infos {
		name := i.Name
		if name == "" {
			name = "(untitled)"
		}
		fmt.Fprintf(stdout, "%s  %s  %s\n", i.ID[:8], i.Cwd, name)
	}
	return 0
}
