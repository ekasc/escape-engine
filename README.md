# escape

Escape's AI agent core. It is a Go runtime for the path from a user prompt to a streamed model response, tool calls, and a settled turn. The repository has two frontends: `engine/` runs the agent, and `shell/` provides the GPUIX UI.

Escape is not a Node runtime and does not load TypeScript extensions. The engine owns providers, tools, sessions, settings, resources, approvals, compaction, and the control protocols used by the shell.

## Current capabilities

- Agent loop: send, stream, call tools, observe, and settle.
- Tools: `bash`, `read`, `write`, `edit`, `grep`, `glob`, `question`, and web search.
- Providers: OpenAI-compatible HTTP, OpenCode Go, OpenCode Zen, Codex OAuth, and a deterministic fake provider.
- Terminal UI: Bubble Tea v2, Bubbles textarea and viewport, Lip Gloss styling, history, completion, session switching, branching, review, diff, status, and compaction commands.
- Sessions: append-only JSONL with a pi-compatible v3 shape.
- Settings: merged global and project configuration, tool selection, provider defaults, reasoning level, approval mode, and session directory.
- Resources: context files, prompt templates, and skills.
- Runtime control: stdio JSON-RPC `serve` mode and the pi-compatible `rpc` command/event mode used by the GPUIX shell.
- Safety: explicit approval requests for side-effecting tools, bounded retries, cancellation, and branch-safe session operations.

## Build

```sh
cd engine
go build -o escape .
# or install the command as `escape`
make install
```

The interactive terminal UI uses Charm's Bubble Tea v2, Bubbles v2, and Lip Gloss v2 packages. Pipes use the line-oriented REPL fallback.

## Usage

```sh
# One coding task
./escape ask --cwd /path/to/project "inspect the failing test and fix it"

# Offline interactive session
./escape repl --fake --cwd /path/to/project

# Resume the newest session for a working directory
./escape repl --fake --cwd /path/to/project --resume

# Require approval for side-effecting tools
./escape repl --cwd /path/to/project --approval ask

# List sessions
./escape list-sessions --cwd /path/to/project

# Choose a provider interactively
./escape login
```

Cache benchmark:

```sh
escape bench-cache --provider opencode-go --model space-bunny-free --count 3
```

The command sends identical requests with a stable prefix and reports provider-reported cache-read tokens. If the provider does not expose cache accounting, it prints `cache_hit_rate=unavailable` instead of reporting a false zero.

`escape login` opens a Huh provider menu with OpenCode Go, OpenCode Zen, and Codex. Use `--provider opencode-go`, `--provider opencode-zen`, or `--provider codex` for non-interactive scripts. If the `opencode` CLI is not installed, Escape prompts for an API key and stores it in `~/.escape/credentials.json`; OpenCode Zen also allows free models without a key. Codex uses the device flow, or `--from-codex` imports an existing Codex login.

Type `/` in the REPL to discover commands. The TUI supports `/diff`, `/review`, `/rename`, `/recap`, `/export`, `/clear`, `/undo`, `/fork`, `/forks`, `/followup`, `/sessions`, `/new`, `/login`, `/status`, `/compact`, `/snapcompact`, and provider controls. The `/login` form is embedded in the TUI; selecting a provider temporarily suspends the TUI while authentication runs.

## Configuration

The engine reads these environment variables:

| Variable | Purpose |
|---|---|
| `ESCAPE_PROVIDER` | `api`, `opencode-go`, `opencode-zen`, or `codex` |
| `ESCAPE_BASE_URL` | OpenAI-compatible API base URL |
| `ESCAPE_API_KEY` | API key for the `api` provider |
| `ESCAPE_MODEL` | Model identifier |
| `ESCAPE_THINKING` | Default reasoning level |
| `ESCAPE_SESSIONS_DIR` | Session storage root |
| `ESCAPE_OAUTH_FILE` | Codex OAuth token file |
| `ESCAPE_OAUTH_ISSUER` | OAuth issuer override |
| `OPENCODE_GO_API_KEY` | OpenCode Go API key override |
| `OPENCODE_ZEN_API_KEY` | OpenCode Zen API key override |
| `ESCAPE_OPENCODE_GO_API_KEY` | Escape-owned OpenCode Go key override |
| `ESCAPE_OPENCODE_ZEN_API_KEY` | Escape-owned OpenCode Zen key override |
| `ESCAPE_CREDENTIALS_FILE` | Escape credential file path |

Settings merge `~/.pi/agent/settings.json` with `<cwd>/.pi/settings.json`. These paths and the JSONL shape remain compatible with the pi ecosystem. Escape-specific environment variables and the `escape` binary do not use the old product names.

## RPC modes

`serve` is the lower-level JSON-RPC surface used by existing integrations:

```sh
./escape serve --session /tmp/session.jsonl --cwd /path/to/project
```

`rpc` is the command/event protocol used by the Escape shell:

```sh
./escape rpc --cwd /path/to/project
```

The shell can start the RPC process without creating a session path. Provider credentials stay inside the Go process.

## Session files

Sessions are append-only JSONL under `~/.pi/agent/sessions/<project-slug>/` by default. Set `ESCAPE_SESSIONS_DIR` to choose another root. The on-disk shape remains compatible with pi-style readers:

- `session` header entries
- `message` entries with `message.role`, `message.content`, and numeric timestamps
- `toolCallId` on tool results
- `session_info.name` for the session title

## Development

```sh
cd engine
gofmt -l .
go vet ./...
go test ./...
go test -race ./...
```

The shell has its own checks:

```sh
cd shell
bun run typecheck
bun run test
bun run build
```

## Layout

```text
engine/
  main.go             CLI and shared wiring
  tui.go              Bubble Tea terminal UI
  internal/agent      Agent loop, events, queue, compaction
  internal/provider   Streaming providers, OpenCode login, and OAuth
  internal/resources  Context, skills, and prompt templates
  internal/rpc        JSON-RPC and pi-compatible command protocols
  internal/session    JSONL session store, branching, export, stats
  internal/settings   Global and project settings
  internal/tools      File, shell, search, and interaction tools
shell/
  app.tsx             GPUIX chat UI
  agent-client.ts     Typed RPC client and local fallback
```
