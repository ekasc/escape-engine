// Command pi-go is a personal agent runtime in Go: a minimal agent loop
// (stream -> tools -> settle) with a REPL and a stdio JSON-RPC serve mode
// that Babylon can talk to. Session files are pi-compatible JSONL.
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

	"github.com/ekasc/pi-go/internal/agent"
	"github.com/ekasc/pi-go/internal/provider"
	"github.com/ekasc/pi-go/internal/rpc"
	"github.com/ekasc/pi-go/internal/session"
	"github.com/ekasc/pi-go/internal/tools"
)

const version = "0.1.0"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "repl":
		return cmdRepl(args[1:], stdin, stdout, stderr)
	case "serve":
		return cmdServe(args[1:], stdin, stdout, stderr)
	case "list-sessions":
		return cmdListSessions(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "pi-go %s\n", version)
		return 0
	case "help", "--help", "-h":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "pi-go: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `pi-go — a personal agent runtime in Go

Usage:
  pi-go repl [--cwd DIR] [--model M] [--fake] [--session PATH]
      Interactive prompt -> stream -> result loop.
  pi-go serve --session PATH --cwd DIR [--model M] [--fake]
      Stdio JSON-RPC control surface (requests on stdin, events+responses
      as JSON lines on stdout). This is what Babylon spawns.
  pi-go list-sessions [--cwd DIR] [--json]
      Recent sessions, newest first.
  pi-go version

Provider configuration (unless --fake):
  AGENT_GO_BASE_URL   e.g. https://api.openai.com/v1 (or DeepSeek/Ollama/vLLM)
  AGENT_GO_API_KEY    API key
  AGENT_GO_MODEL      model id (default gpt-4o-mini)

Sessions are stored as pi-compatible JSONL under AGENT_GO_SESSIONS_DIR
(default ~/.pi/agent/sessions), readable by Babylon unchanged.
`)
}

// --- shared wiring ---

type providerConfig struct {
	fake    bool
	baseURL string
	apiKey  string
	model   string
	maxTok  int
}

func (c providerConfig) provider() (provider.Provider, error) {
	if c.fake {
		return &provider.Fake{Handler: fakeHandler}, nil
	}
	if c.apiKey == "" {
		return nil, errors.New("no provider configured: set AGENT_GO_API_KEY (and AGENT_GO_BASE_URL / AGENT_GO_MODEL) or use --fake")
	}
	return provider.NewOpenAI(c.baseURL, c.apiKey, c.model), nil
}

// fakeHandler is the offline demo model: it echoes the last user text and
// never calls tools.
func fakeHandler(ctx context.Context, req provider.Request) ([]provider.Event, error) {
	var last string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			last = req.Messages[i].Text
			break
		}
	}
	return []provider.Event{
		{Kind: provider.EventText, Text: "[fake model] " + last},
		{Kind: provider.EventDone, StopReason: "stop"},
	}, nil
}

func commonProviderFlags(fs *flag.FlagSet, c *providerConfig) {
	fs.BoolVar(&c.fake, "fake", false, "use the offline fake model (no network)")
	fs.StringVar(&c.baseURL, "base-url", envOr("AGENT_GO_BASE_URL", "https://api.openai.com/v1"), "OpenAI-compatible base URL")
	fs.StringVar(&c.apiKey, "api-key", os.Getenv("AGENT_GO_API_KEY"), "API key (or AGENT_GO_API_KEY)")
	fs.StringVar(&c.model, "model", envOr("AGENT_GO_MODEL", "gpt-4o-mini"), "model id (or AGENT_GO_MODEL)")
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
	if flagPath != "" {
		abs, err := filepath.Abs(flagPath)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	return session.NewPath(session.DefaultRoot(), cwd)
}

// --- repl ---

func cmdRepl(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cwd string
	var sessionPath string
	var cfg providerConfig
	fs.StringVar(&cwd, "cwd", "", "working directory (default: current)")
	fs.StringVar(&sessionPath, "session", "", "session file (default: new under sessions root)")
	commonProviderFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	p, err := cfg.provider()
	if err != nil {
		fmt.Fprintln(stderr, "pi-go:", err)
		return 1
	}
	path, err := resolveSessionPath(sessionPath, cwd)
	if err != nil {
		fmt.Fprintln(stderr, "pi-go:", err)
		return 1
	}
	store, err := session.Open(path, cwd)
	if err != nil {
		fmt.Fprintln(stderr, "pi-go:", err)
		return 1
	}
	defer store.Close()

	ag := agent.New(agent.Options{
		Store:    store,
		Provider: p,
		Tools:    tools.Default(cwd),
		Cwd:      cwd,
		Model:    cfg.model,
	})

	fmt.Fprintf(stdout, "pi-go %s — %s\nsession: %s\n(/help, /stop, /exit)\n\n", version, cfg.model, path)
	return replLoop(ag, stdin, stdout)
}

func replLoop(ag *agent.Agent, stdin io.Reader, stdout io.Writer) int {
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
		case line == "/state":
			st := ag.State()
			fmt.Fprintf(stdout, "[state: %s turn=%s model=%s settle=%s]\n", st.State, st.TurnID, st.Model, st.SettleReason)
		case line == "/help":
			fmt.Fprintln(stdout, "/help /stop /state /exit")
		default:
			runTurnInteractive(ag, line, stdout)
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
		fmt.Fprintln(stderr, "pi-go serve: --session PATH is required")
		return 2
	}

	p, err := cfg.provider()
	if err != nil {
		fmt.Fprintln(stderr, "pi-go:", err)
		return 1
	}
	path, err := filepath.Abs(sessionPath)
	if err != nil {
		fmt.Fprintln(stderr, "pi-go:", err)
		return 1
	}
	store, err := session.Open(path, cwd)
	if err != nil {
		fmt.Fprintln(stderr, "pi-go:", err)
		return 1
	}
	defer store.Close()

	ag := agent.New(agent.Options{
		Store:    store,
		Provider: p,
		Tools:    tools.Default(cwd),
		Cwd:      cwd,
		Model:    cfg.model,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := rpc.NewServer(ag, store, session.DefaultRoot(), cwd, stdout)
	events := ag.Events()
	defer ag.Unsubscribe(events)
	go srv.Relay(ctx, events)

	// Serve until stdin closes (the parent's lifetime) or we're signaled.
	err = srv.Serve(ctx, stdin)
	if err != nil {
		fmt.Fprintln(stderr, "pi-go serve:", err)
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
		fmt.Fprintln(stderr, "pi-go:", err)
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
