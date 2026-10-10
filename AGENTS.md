
## Project layout

Wire- and UI-compatible reimplementation of the Claude Code CLI, targeting
claude 2.1.296. See README.md for build/run/test and the internal/ package
map. `audits/*.md` hold parity audit notes (written against 2.1.294 —
treat version-specific numbers as historical). Real-wire captures and
extracted reference source stay local per .gitignore (Anthropic IP).

## Version retarget runbook (294→295, proven)

When the installed `claude` self-updates, the diff harness goes red with
`cc_version`/`user-agent` diffs. To retarget:

1. Extract the new binary — modules are plaintext JS between
   `// @bun @bytecode` markers, each ending at the next `\x00`:
   `python3` carve loop over `~/.local/share/claude/versions/<ver>`
   → `~/Code/claude-extracted-<ver>/mod_NNNN.js` (2356 for 295).
2. `CLAUDE_GO_DIFF_DUMP=/tmp/dump go test -tags e2e -run 'TestDiffScenarios' ./e2e/`
   — real's exact new wire lands in `/tmp/dump/real-goldenN.json`.
3. Apply deltas: version literals (`api.Version`, `agent.version()`,
   `mcp` clientInfo, `session` transcript stamps, `serve` serverInfo,
   `repl` banner), then any text deltas (295: system_main.txt opening
   line, builtin_skills.txt plugin-authoring desc).
4. Re-run the diff harness until 0 field diffs on all 4 scenarios.

## Verification

- `go test ./internal/... .` — unit
- `go test -tags e2e -count=1 ./e2e/` — full e2e (~230s)
- `go test -tags e2e -count=1 -run 'TestDiff' ./e2e/ -v` — wire diff
- `go vet ./...`
