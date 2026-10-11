# claude-go

A from-scratch Go reimplementation of the Claude Code CLI, targeting
wire- and UI-level parity with `claude` 2.1.296. One static binary, no
runtime dependencies.

**Not affiliated with or endorsed by Anthropic.** This is a clean-room
compatibility clone for study and personal use. No Anthropic-authored
text (system prompts, tool schemas) ships in this repository — see
[Extraction](#extraction) below and [License](#license).

## Requirements

- Go 1.26+
- A real `claude` install is *optional* — needed for `extract`
  (full-fidelity prompts/schemas) and the differential wire tests
  (`TestDiff*`)

## Build

```sh
make build          # go build ./...
go build -o claude-go .
```

## Quickstart

```sh
./claude-go extract         # optional: harvest real prompts/schemas from your claude install
./claude-go --mock          # run against the built-in mock API — no credentials needed
./claude-go -p "say hi"     # print mode
./claude-go login           # OAuth login (or: setup-token for a long-lived token)
./claude-go                 # interactive REPL
```

Credentials live in `~/.claude-go/.credentials.json` (override with
`CLAUDE_GO_CREDS_FILE`) or the macOS Keychain, so the binary can borrow
an existing `claude` login without sharing a write path.

## Subcommands

```
agents            list agent types + background sessions
attach <id>       attach to a running background session
daemon …          background-session supervisor (run|start|status|stop|install|uninstall)
stop|kill <id>    stop a background session
logs <id>         stream a background session's output
rm <id>           remove a stopped session's state
respawn <id>      respawn a dead background worker
auth …            login|status|logout
config get|set|list
doctor            diagnostics
extract           harvest real prompts/schemas from a local claude install
import <file>     import a JSONL transcript
login             OAuth login
mcp …             list|add|add-json|add-from-claude-desktop|get|remove|
                  login|logout|reset-project-choices|serve
plugin …          list|install|remove|marketplace|init|tag|eval
purge             delete local project state
sandbox trust     trust the sandbox MITM CA
setup-token       store a long-lived token
install, update   rebuild from source (git pull + go build)
remote-control    control-plane server (skeleton)
bg-host           (internal) background-session pty host
```

`claude-go --help` lists the full flag surface. Note: several user-facing
strings intentionally match real `claude` output (e.g. `claude attach`)
for drop-in compatibility.

## Testing

```sh
make test          # go test ./... — unit tests
make test-e2e      # full end-to-end suite (~5 min, no credentials needed)
make test-all      # vet + unit + e2e
```

The e2e suite builds the real binary once, then drives it through PTY and
an in-process mock Anthropic API (`internal/mock`). It covers print-mode
output formats, interactive rendering, slash commands, tools, hooks,
sessions, subagents, and the flag/subcommand surface.

**Wire diff harness** — the core parity check. `TestDiffScenarios` runs
*both* real `claude` and claude-go against the same mock and structurally
diffs the streamed `/v1/messages` request bodies. Real `claude` is found
via `CLAUDE_REAL_BIN`, `~/.local/bin/claude`, or PATH; tests skip
gracefully without it.

```sh
go test -tags e2e -run 'TestDiff' ./e2e/ -v      # 4 scenarios, expects 0 field diffs
CLAUDE_GO_DIFF_DUMP=/tmp/dump go test -tags e2e -run 'TestDiffScenarios' ./e2e/
```

## Architecture

| Package | Role |
|---|---|
| `internal/api` | Anthropic API client, SSE streaming, providers (Bedrock/Vertex), uTLS fingerprinting |
| `internal/agent` | Agentic loop, compaction, subagents, system-prompt assembly |
| `internal/tools` | Builtin toolset: bash/file/search/web, MCP bridge, sandbox, cron, tasks |
| `internal/repl` | Terminal UI: editor, keybindings, dialogs, slash commands |
| `internal/session` | JSONL transcript store, resume/rewind, picker |
| `internal/mcp` | MCP client (stdio/sse/http/ws) + `mcp serve` |
| `internal/auth` | Credentials: keychain, `~/.claude-go` creds file, OAuth refresh |
| `internal/config` | `~/.claude.json` + settings.json handling |
| `internal/perms` | Permission model |
| `internal/hooks` | settings.json hook dispatch |
| `internal/plugins` | Plugin + marketplace loading |
| `internal/bg` | Background sessions: PTY spawn, daemon (launchd/systemd), attach |
| `internal/bridge` | Remote-control / teleport / cloud wire surface |
| `internal/browsermcp` | Claude-in-Chrome MCP server + native-messaging host |
| `internal/netproxy` | Sandbox egress MITM proxy + CA |
| `internal/mock` | Fake Anthropic API used by `--mock`, e2e, and `cmd/mockd` |
| `internal/extract` | Optional harvest of real's bundled text (prompts, tool schemas) from a local `claude` install |
| `internal/lsp`, `internal/telemetry`, `internal/voice` | LSP client, telemetry, push-to-talk |

`cmd/mockd` runs the mock API as a standalone daemon on `127.0.0.1:8898`
so real `claude` can complete turns against it for wire captures.

## Development

- **`AGENTS.md`** — the version-retarget runbook: when real `claude`
  self-updates, this is the proven carve→dump→patch loop that re-aligns
  version stamps and text deltas.
- **`audits/`** — parity audit notes per surface (flags/env, hooks/perms,
  TUI, wire-stream, slash, tools, session/MCP). They cite extracted
  module IDs (`mod_NNNN.js`) from a local carve of the real binary;
  the reference trees themselves are gitignored (Anthropic IP).
- Golden-wire tests (`TestGolden*`) compare request structure against
  captures regenerated by `claude-go extract` — fixtures are not
  committed (Anthropic content).

## Extraction

No Anthropic-authored prompt text or tool schemas ship in this repo.
The binary ships minimal community-authored fallbacks so `--mock`, tests,
and basic use work standalone. For byte-level wire parity, run:

```sh
./claude-go extract
```

This finds your local `claude` (CLAUDE_REAL_BIN → `~/.local/bin/claude` →
PATH), runs it once per model class against the in-process mock API, and
caches the captured `system` prompts + `tools` schemas in
`~/.claude-go/extracted/` — the text never leaves your machine. The
binary prefers extracted text when present. Re-run after `claude`
self-updates.

## License

MIT — see [LICENSE](LICENSE). The license applies to the Go code in this
repository. Claude Code prompt text, tool schemas, and other behavioral
artifacts remain Anthropic's intellectual property — none ship in this
repository; `claude-go extract` derives them locally from the user's own
`claude` install at runtime. This project is unaffiliated with
Anthropic.
