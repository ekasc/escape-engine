# pi-go

A personal agent runtime in Go. A clean-slate engine: prompt → streaming
model call → tool calls → observe → settle. No Node.js, no bundling, no
parity debt — a single small binary you own.

This is **not** a port of pi. It follows the plan in
[`docs/HANDOFF.md`](docs/HANDOFF.md): own the runtime, drop Node, keep
startup and size tiny, control the agent loop.

## Status: v1 (per the handoff's cut line)

| In | Out |
|---|---|
| Agent loop: send → stream → tools → settle (`agent_settled`) | extensions / skills |
| Tools: `bash`, `read`, `write`, `edit` (opencode-style patch), `grep`, `glob` | MCP servers |
| Session persistence: pi-compatible append-only JSONL | thinking levels |
| CLI REPL | compaction |
| Stdio JSON-RPC `serve` (send/steer/stop/state/list-sessions + event stream) | provider routing file |
| Graceful abort mid-turn, bounded provider retries, empty tool results | images, multi-agent |

## Build

```sh
go build -o pi-go .
```

Stdlib only — no dependencies, no `go.sum`.

## Usage

```sh
# Interactive REPL (offline demo with a fake model)
./pi-go repl --fake --cwd /path/to/project

# REPL against a real OpenAI-compatible endpoint
export AGENT_GO_BASE_URL=https://api.deepseek.com/v1
export AGENT_GO_API_KEY=sk-...
export AGENT_GO_MODEL=deepseek-chat
./pi-go repl --cwd /path/to/project

# Stdio JSON-RPC serve mode (what Babylon spawns)
./pi-go serve --session /tmp/session.jsonl --cwd /path/to/project

# Recent sessions
./pi-go list-sessions [--cwd DIR] [--json]
```

## Stdio JSON-RPC protocol

One JSON object per line on each side. Requests on stdin; responses and
events on stdout (responses carry `"jsonrpc"` + the request id, events carry
`"type":"event"`).

**Methods:** `send {text}`, `steer {text}`, `stop`, `state`, `list-sessions`,
`ping`.

**Events:** `session_started`, `turn_started`, `message_start`,
`message_delta`, `message_end`, `tool_call`, `tool_result`,
`agent_settled {reason: done|stopped|error}`, `error`.

Process model: one `serve` subprocess per session, spawned by the parent.
When stdin closes, the current turn is stopped and events are flushed before
exit.

## Session files

Append-only JSONL in `~/.pi/agent/sessions/<slug>/<timestamp>_<id>.jsonl`,
matching pi's v3 shape so Babylon's reader (`electron/sessions.ts` →
`readSessionTail`/`readSessionRange`/`readSessionInfo`) reads them with zero
changes: `session` header, `message` entries with
`message.{role,content,timestamp}`, `toolCallId` on tool results,
`session_info.name` for the title. Override the root with
`AGENT_GO_SESSIONS_DIR`.

## Layout

```
main.go               CLI: repl / serve / list-sessions / version
internal/session      JSONL store + tail/range/info reader (the shared contract)
internal/provider     streaming OpenAI-compatible client + scriptable fake
internal/tools        bash, read, write, edit, grep, glob
internal/agent        the loop: state machine, events, send/steer/stop
internal/rpc          stdio JSON-RPC server
```

## Tests

```sh
go test ./...          # unit tests incl. state-machine tests (fake provider, no network)
go test -race ./...
```
