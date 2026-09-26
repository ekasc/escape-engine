# Escape engine handoff

**Status:** Active implementation

## Purpose

`engine/` is Escape's AI agent core. It owns the model loop, tools, providers, sessions, settings, resources, approvals, compaction, and the protocols consumed by the GPUIX shell in `../shell`.

Escape is a Go runtime. It does not load Node.js extensions or depend on a JavaScript runtime.

## Runtime contract

The engine exposes two stdio interfaces:

- `escape serve` speaks the lower-level JSON-RPC control surface.
- `escape rpc` speaks the command/event JSON-lines protocol used by the GPUIX shell.

The shell starts the engine as a child process. Provider credentials remain in the Go process. Prompts, streamed text, tool activity, approval requests, questions, and settlement events cross the process boundary as JSON lines.

## Session contract

Sessions are append-only JSONL. The format keeps the fields expected by pi-compatible readers:

- `session` header with an ID, version, timestamp, and working directory.
- `message` entries with an ID, parent ID, role, content, and numeric timestamp.
- Text and tool-call content blocks.
- `toolCallId` on tool results.
- `session_info` entries for titles and recaps.

The default storage root is `~/.pi/agent/sessions`. Set `ESCAPE_SESSIONS_DIR` to use another root.

## Configuration

The engine reads merged settings from `~/.pi/agent/settings.json` and `<cwd>/.pi/settings.json`. Escape-specific process settings use these variables:

- `ESCAPE_PROVIDER`
- `ESCAPE_BASE_URL`
- `ESCAPE_API_KEY`
- `ESCAPE_MODEL`
- `ESCAPE_THINKING`
- `ESCAPE_SESSIONS_DIR`
- `ESCAPE_OAUTH_FILE`
- `ESCAPE_OAUTH_ISSUER`

## Package boundaries

- `internal/agent` owns the turn state machine and event bus.
- `internal/provider` owns streaming providers, OpenCode login, and Codex OAuth.
- `internal/tools` owns tool implementations and approval boundaries.
- `internal/session` owns JSONL persistence, branching, export, and statistics.
- `internal/settings` owns global and project configuration.
- `internal/resources` owns context files, skills, and prompt templates.
- `internal/rpc` owns the two stdio protocols.
- `tui.go` owns the terminal frontend.
- `../shell` owns the GPUIX frontend and its typed RPC client.

## Next work

The next bounded slice should connect the GPUIX shell to live tool, approval, and question events. Add a test that starts `escape rpc --fake`, drives `EscapeAgentClient`, and verifies the event sequence before adding UI controls.
