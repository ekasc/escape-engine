# HANDOFF — Personal Go Agent Runtime (separate project)

**Status:** Pre-scaffold. This is the handoff for a NEW project, owned by
`ekasc`, separate from Babylon. Babylon (this repo) stays as the GUI surface.

**Date:** 2026-08-14
**Author:** ekasc (via Babylon session)

---

## 1. What this project is

A **personal agent runtime written in Go** — a clean-slate engine that does
exactly what the owner needs, with **no Node.js, no bundling, no parity debt,
no extension ecosystem to preserve**.

It is **NOT** a port of pi (`@earendil-works/pi-coding-agent`). The owner
explicitly does not want behavioral parity with pi — they want their own
engine. pi's 48k LOC / 15 MB runtime and its JS extension ecosystem are
explicitly out of scope.

**Motivations (as stated by the owner):** own the runtime; drop Node;
single-binary; startup/size; control over the agent loop. This is a
learning/ownership project, not a product requirement.

## 2. Non-negotiable context (decisions already made)

- **Babylon stays on Electron + pi.** The Wails analysis concluded Babylon's
  backend is pi (Node, in-process `ModelRuntime`), so Babylon does not move.
  This Go runtime is a sibling project that Babylon *may* talk to later.
- **Interface to Babylon (when it happens):** spawn the Go binary as a
  **subprocess over stdio JSON-RPC** — the same pattern Babylon already uses
  for workflow workers and MCP. No cgo, no embedded sidecar.
- **Session files must be JSONL** so Babylon's existing reader
  (`electron/sessions.ts` → `readSessionTail`/`readSessionRange`, tail-first
  byte-offset windows) can read them with zero changes.
- **Owner register:** focused, fast, precise; an instrument, not a toy.
  Simplicity first (AGENTS.md). No speculative features.

## 3. Project decisions to make first

1. **From scratch vs build on opencode.** opencode (already installed) is a
   Go agent CLI; pi's `opencode-go` provider shells out to it for model
   routing. Paths:
   - **Scratch** (recommended for the owner's stated goal): full ownership,
     best learning; ~weeks to a usable v1.
   - **Extend opencode's Go core**: fastest to useful; inherits provider
     support; less clean ownership.
   Decision: **START FROM SCRATCH, borrow patterns from opencode.**

2. **Provider scope for v1:** ONE provider (OpenAI-compatible or
   Anthropic-compatible HTTP) via `net/http`. The model catalog cache pattern
   (see §7, alibaba) is a v2 concern.

3. **Repo:** new repo (e.g. `github.com/ekasc/agent-go` or similar — owner
   decides). Move this file into it.

## 4. v1 scope (cut line)

**In:**
- Agent loop: user prompt → model call (streaming) → tool calls → observe →
  loop until done. `agent_settled` semantics.
- Tools: `bash` (`os/exec`), file `read`, `write`, `edit` (patch-style,
  copy opencode's edit tool), maybe `grep`/`glob`.
- Session persistence: append-only JSONL (own format, pi-compatible fields —
  see §5).
- CLI: minimal REPL (prompt → stream → result), no TUI.
- Stdio JSON-RPC control surface (send/steer/stop + event stream) — this is
  what Babylon will talk to; build it early even if the CLI is the demo.
- Graceful handling of: abort/stop mid-turn, provider errors with retry
  (bounded), empty tool results.

**Out (v1):** extensions, skills loading, MCP servers, thinking levels,
compaction, provider routing file, auto-retry heuristics, images, multi-agent.

## 5. Session file format (must stay readable by Babylon)

pi-style JSONL: one JSON object per line, append-only, in
`~/.pi/agent/sessions/<project-slug>/<timestamp>_<id>.jsonl` (or own layout —
**keep the JSONL line shape compatible**):

```json
{"type":"message","id":"...","parentId":"...","timestamp":"2026-08-14T…Z","message":{"role":"user","content":"…","timestamp":1784702694866}}
{"type":"message","id":"…","message":{"role":"assistant","content":[{"type":"text","text":"…"}],"model":"…","timestamp":…}}
{"type":"session_info","id":"…","timestamp":"…","name":"Auto title"}
```

Compatibility rules Babylon's reader depends on:
- `entry.type === "message"` with `entry.message` carrying `role`,
  `content` (string or array of `{type:"text",text}` blocks), and numeric
  `timestamp`.
- `entryId` = `entry.id` (Babylon keys messages by it).
- `session_info` entries with a `name` field (Babylon shows it as the chat
  title; `getSessionName` stops at the first `session_info` from the end).
- Tail reads: Babylon opens O(tail) not O(file) — it reads the last ~2 MB
  aligned to line boundaries. Don't write giant single-line blobs if tool
  output can be avoided; if unavoidable, that's OK (Babylon clamps at 16 KB
  per tool result and reads the rest on demand).

## 6. Stdio JSON-RPC protocol sketch (for the Babylon bridge later)

Process model: Babylon spawns `agent-go serve --session <path> --cwd <dir>`
once per session; events stream on stdout as JSONL; control messages go on
stdin (JSON-RPC 2.0).

**Commands (request → response):**
- `send` `{text}` → starts a turn
- `steer` `{text}` → interrupt + redirect (mid-turn)
- `stop` → abort the current turn
- `list-sessions` → recent sessions for the cwd
- `state` → current status/model/turn info

**Events (stdout, one JSON per line):**
- `session_started` `{sessionId, sessionFile, cwd}`
- `message_start` / `message_delta` `{role:"assistant", text}` (streaming)
- `message_end` `{role, model, timestamp}` (entry committed)
- `tool_call` `{toolCallId, name, args}` / `tool_result` `{toolCallId, output, error?}`
- `agent_settled` `{reason: "done"|"stopped"|"error"}`
- `error` `{message}`

Do NOT mirror prompts/tool data to logs in production (Babylon strips renderer
console for the same reason).

## 7. Patterns worth stealing (learned in Babylon/pi work)

- **Tail-first session reads:** never O(file) opens; read last N bytes aligned
  to newlines, fetch older windows on scroll. Babylon open: 27 ms vs 137 ms.
- **Optimistic everything:** UI never waits on the host; failure restores
  state. Same principle applies to the agent loop's event stream.
- **Cheap model for metadata:** pi's auto-naming uses a cheap model
  (`deepseek-v4-flash`, `reasoning:"low"`, maxTokens ~200) for one-shot,
  non-blocking metadata (titles, recaps). The Go runtime can do the same
  via `ModelRuntime`-equivalent HTTP calls.
- **Model catalog caching:** pi's alibaba extension caches the catalog fetch
  with a 4h TTL + stale-ctx guard. Any remote catalog in Go: cache with TTL,
  never block the first prompt on a cold fetch.
- **`opencode-go` provider:** pi resolves `getModel("opencode-go", …)` by
  shelling out to the opencode CLI. That's the existing Go interop precedent.
- **Milestones:** threads extension reports milestone events; a "report
  milestone" tool injected into the session. Useful pattern for the Go
  runtime if long-running tasks appear.
- **Verification discipline (AGENTS.md §5):** never claim from inspection;
  run tests, typecheck, build, and a headless launch. Babylon uses
  `PIDECK_HEADLESS=1` + CDP for UI verification without disturbing the user.

## 8. Anti-goals / traps

- Do NOT add every pi feature "just in case" — the whole point is a minimal
  engine you own. Every speculative feature is the project dying slowly.
- Do NOT try to load pi extensions (they're JS).
- Do NOT aim for behavioral parity on compaction/retries/thinking — decide
  what you need as you need it.
- Do NOT couple the Go runtime to Babylon's repo (separate project). Babylon
  talks to it only through the stdio protocol + shared JSONL format.

## 9. Useful references on this machine

- pi's own example runtime (Node, for protocol ideas):
  `~/.vite-plus/js_runtime/node/26.3.0/lib/node_modules/@earendil-works/pi-coding-agent/examples/sdk/13-session-runtime.ts`
- pi docs: `docs/sessions.md`, `docs/sdk.md`, `docs/session-format.md` under
  the same package; `.pi-docs/` copies in the Babylon repo.
- opencode source (Go patterns to borrow): `~/.pi/agent/npm/node_modules/opencode-go/…` (and the `opencode` CLI on PATH).
- Babylon's session reader (the contract to stay compatible with):
  `electron/sessions.ts` in the Babylon repo.
- Babylon's subprocess/spawn patterns: `electron/workflows.ts`,
  `electron/subagents.ts`.

## 10. First steps when starting

1. `go mod init` the new repo; copy this file in as `docs/HANDOFF.md`.
2. Implement the session JSONL writer/reader + tests first (it's the shared
   contract).
3. Implement one provider (streaming HTTP) with a fake-model test mode.
4. Implement the loop: send → stream → tools → settle. Unit-test the state
   machine (spawn/fake tools, no network).
5. Implement `bash` + file tools with the opencode edit semantics.
6. CLI REPL, then the stdio JSON-RPC `serve` command.
7. When the serve command is stable: wire Babylon to it behind a flag
   (`AGENT_GO_BIN` env), keeping pi as the default runtime.
