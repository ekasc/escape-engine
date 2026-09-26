# Escape engine status

This document tracks the remaining work in Escape's AI agent core. The engine is the runtime; the GPUIX application in `../shell` is one frontend.

The complete CLI-to-GUI inventory lives in [`../FEATURE_MAP.md`](../FEATURE_MAP.md).

## Implemented

- Agent loop with streaming provider responses, tool execution, retries, cancellation, steering, and follow-up messages.
- File and shell tools, image reads, web search, questions, and approval requests.
- OpenAI-compatible HTTP, OpenCode Go, OpenCode Zen, Codex OAuth, and fake providers.
- Global and project settings with nested merging, provider defaults, reasoning levels, tool selection, approval mode, and configurable session roots.
- Context files, prompt templates, and skills.
- Append-only JSONL sessions with titles, usage statistics, export, compaction, branch summaries, forking, cloning, and undo.
- Bubble Tea terminal UI with daily-driver commands and history.
- `serve` JSON-RPC mode and the `rpc` command/event mode used by the GPUIX shell.

## Storage

Escape reads and writes only its own tree. Configuration, sessions, skills, and context files live under `~/.escape` and `<project>/.escape`, and nothing is written outside them.

Earlier drafts shared another tool's directories and on-disk shape. That was a mistake: it made Escape's startup cost depend on another program's activity, put files in the store that Escape could not explain, and made it impossible to tell whose session a given file was. Escape took design inspiration from that tool; it shares no storage with it.

## Known gaps

1. `escape login` provides a Huh menu for OpenCode Go, OpenCode Zen, and Codex, but the OpenCode flows delegate to the installed CLI.
2. The GPUIX shell parses engine tool, approval, and question events but does not render or resolve them yet.
3. The shell has no end-to-end test that drives `EscapeAgentClient` against a live `escape rpc` process.
4. The engine's model catalog is static. Provider discovery and context-window metadata need a deliberate source of truth.
5. The active goal is paused at its turn limit. It must be resumed with a bounded slice and fresh verification.

## Verification target

Every slice should leave these checks green:

```sh
cd engine
gofmt -l .
go vet ./...
go test ./...
go test -race ./...

cd ../shell
bun run typecheck
bun run test
bun run build
```

A frontend slice also needs an `agent-browser` smoke flow against the running GPUIX browser build.
