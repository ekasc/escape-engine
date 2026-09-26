package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/ekasc/escape-engine/internal/provider"
	"github.com/ekasc/escape-engine/internal/settings"
)

type loginProvider string

const (
	loginOpenCodeGo  loginProvider = "opencode-go"
	loginOpenCodeZen loginProvider = "opencode-zen"
	loginCodex       loginProvider = "codex"
)

type tuiLoginResultMsg struct {
	provider string
	err      error
}

type providerLoginCommand struct {
	target string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func (c *providerLoginCommand) SetStdin(r io.Reader)  { c.stdin = r }
func (c *providerLoginCommand) SetStdout(w io.Writer) { c.stdout = w }
func (c *providerLoginCommand) SetStderr(w io.Writer) { c.stderr = w }

func (c *providerLoginCommand) Run() error {
	if err := runProviderLogin(context.Background(), c.target, c.stdin, c.stdout, c.stderr, false, provider.ChatGPTClientID); err != nil {
		return err
	}
	return settings.SetGlobalProvider(c.target)
}

func loginProviderLabel(name string) string {
	switch normalizeLoginProvider(name) {
	case string(loginOpenCodeGo):
		return "OpenCode Go"
	case string(loginOpenCodeZen):
		return "OpenCode Zen"
	case string(loginCodex):
		return "Codex"
	default:
		return name
	}
}

func normalizeLoginProvider(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "opencode-go", "opencode_go", "go":
		return string(loginOpenCodeGo)
	case "opencode-zen", "opencode_zen", "zen", "opencode":
		return string(loginOpenCodeZen)
	case "codex", "openai", "chatgpt":
		return string(loginCodex)
	default:
		return strings.TrimSpace(name)
	}
}

func newLoginProviderForm(selected *string) *huh.Form {
	field := huh.NewSelect[string]().
		Title("Choose a provider").
		Description("Escape will use this provider for future sessions.").
		Options(
			huh.NewOption("OpenCode Go", string(loginOpenCodeGo)),
			huh.NewOption("OpenCode Zen", string(loginOpenCodeZen)),
			huh.NewOption("Codex", string(loginCodex)),
		).
		Value(selected)
	return huh.NewForm(huh.NewGroup(field)).
		WithTheme(huh.ThemeFunc(huh.ThemeCharm)).
		WithWidth(80).
		WithHeight(10).
		WithShowHelp(true)
}

func chooseLoginProvider(input io.Reader, output io.Writer, accessible bool) (string, error) {
	selected := string(loginOpenCodeGo)
	form := newLoginProviderForm(&selected).
		WithInput(input).
		WithOutput(output).
		WithAccessible(accessible).
		WithViewHook(func(view tea.View) tea.View {
			view.AltScreen = true
			return view
		})
	if err := form.Run(); err != nil {
		return "", err
	}
	return normalizeLoginProvider(selected), nil
}

func (m *tuiModel) openLoginForm() tea.Cmd {
	m.loginSelection = string(loginOpenCodeGo)
	m.loginForm = newLoginProviderForm(&m.loginSelection)
	m.status = "choose a provider"
	m.updateLayout()
	return m.loginForm.Init()
}

func (m *tuiModel) updateLoginForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	updated, cmd := m.loginForm.Update(msg)
	if form, ok := updated.(*huh.Form); ok {
		m.loginForm = form
	}

	switch m.loginForm.State {
	case huh.StateCompleted:
		selected := normalizeLoginProvider(m.loginSelection)
		m.loginForm = nil
		m.setBusy(true)
		m.status = "connecting to " + loginProviderLabel(selected)
		return m, tea.Exec(
			&providerLoginCommand{target: selected},
			func(err error) tea.Msg {
				return tuiLoginResultMsg{provider: selected, err: err}
			},
		)
	case huh.StateAborted:
		m.loginForm = nil
		m.setBusy(false)
		m.status = "ready"
		m.appendLine("login canceled", true)
	}
	return m, cmd
}

func (m *tuiModel) applyTUIProvider(name string) error {
	model := ""
	switch normalizeLoginProvider(name) {
	case string(loginOpenCodeGo):
		model = "minimax-m3"
	case string(loginOpenCodeZen):
		model = provider.OpenCodeZenDefaultModel
	case string(loginCodex):
		model = "gpt-5.6-luna"
	}
	cfg := providerConfig{
		providerName:  name,
		model:         model,
		thinkingLevel: m.reasoning,
	}
	p, err := cfg.provider()
	if err != nil {
		return err
	}
	m.agent.SetProvider(p)
	return nil
}

func promptForOpenCodeKey(target string, input io.Reader, output io.Writer, required bool) error {
	key := ""
	field := huh.NewInput().
		Title("OpenCode Go API key").
		Description("The OpenCode CLI is not installed. Paste a key to continue. It will be stored in ~/.escape/credentials.json.").
		EchoMode(huh.EchoModePassword).
		Value(&key)
	if target == string(loginOpenCodeZen) {
		field = huh.NewInput().
			Title("OpenCode Zen API key").
			Description("The OpenCode CLI is not installed. Paste a key, or press enter to use free models.").
			EchoMode(huh.EchoModePassword).
			Value(&key)
	} else if required {
		field = field.Validate(huh.ValidateNotEmpty())
	}
	form := huh.NewForm(huh.NewGroup(field)).
		WithInput(input).
		WithOutput(output).
		WithTheme(huh.ThemeFunc(huh.ThemeCharm)).
		WithWidth(80).
		WithHeight(9)
	if err := form.Run(); err != nil {
		return err
	}
	if strings.TrimSpace(key) == "" {
		if required {
			return fmt.Errorf("an OpenCode Go API key is required")
		}
		return nil
	}
	return provider.SaveOpenCodeKey(target, key)
}

func runProviderLogin(ctx context.Context, target string, input io.Reader, output, errOutput io.Writer, fromCodex bool, clientID string) error {
	switch target {
	case string(loginOpenCodeGo), string(loginOpenCodeZen):
		loginTarget := provider.OpenCodeLoginGo
		if target == string(loginOpenCodeZen) {
			loginTarget = provider.OpenCodeLoginZen
		}
		if err := provider.LoginOpenCode(ctx, loginTarget, input, output, errOutput); err != nil {
			if !errors.Is(err, provider.ErrOpenCodeCLIMissing) {
				return err
			}
			return promptForOpenCodeKey(target, input, output, target == string(loginOpenCodeGo))
		}
		return nil
	case string(loginCodex):
		if fromCodex {
			tokens, err := provider.ImportCodexTokens()
			if err != nil {
				return err
			}
			return tokens.Save()
		}
		_, err := provider.DeviceLogin(ctx, clientID, func(line string) {
			if output != nil {
				fmt.Fprintln(output, line)
			}
		})
		return err
	default:
		return fmt.Errorf("unknown provider %q", target)
	}
}

func cmdLogin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	providerFlag := fs.String("provider", "", "provider to configure: opencode-go, opencode-zen, or codex")
	clientID := fs.String("client-id", provider.ChatGPTClientID, "Codex OAuth client id")
	fromCodex := fs.Bool("from-codex", false, "import existing tokens from ~/.codex/auth.json")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	selected := normalizeLoginProvider(*providerFlag)
	if selected == "" {
		if !interactiveInput(stdin) {
			fmt.Fprintln(stderr, "escape login: interactive terminal required; use --provider opencode-go|opencode-zen|codex")
			return 2
		}
		var err error
		selected, err = chooseLoginProvider(stdin, stdout, false)
		if err != nil {
			if err == huh.ErrUserAborted {
				fmt.Fprintln(stdout, "escape login: canceled")
				return 1
			}
			fmt.Fprintf(stderr, "escape login: provider selection failed: %v\n", err)
			return 1
		}
	}
	if selected != string(loginOpenCodeGo) && selected != string(loginOpenCodeZen) && selected != string(loginCodex) {
		fmt.Fprintf(stderr, "escape login: unknown provider %q (choose opencode-go, opencode-zen, or codex)\n", selected)
		return 2
	}

	ctx := context.Background()
	fmt.Fprintf(stdout, "escape login: connecting to %s...\n", loginProviderLabel(selected))
	if err := runProviderLogin(ctx, selected, stdin, stdout, stderr, *fromCodex, *clientID); err != nil {
		fmt.Fprintf(stderr, "escape login: %v\n", err)
		return 1
	}

	if err := settings.SetGlobalProvider(selected); err != nil {
		fmt.Fprintf(stderr, "escape login: save provider selection: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "escape login: %s is now the default provider\n", loginProviderLabel(selected))
	return 0
}
