//go:build e2e

// diff_scenarios_test.go — the scenario table: identical inputs driven
// through real claude and claude-go, request streams diffed.
// Run: go test -tags e2e -run 'TestDiff' ./e2e/ -v

package e2e

import "testing"

var diffScenarios = []diffScenario{
	{
		name: "print-say-hi",
		args: []string{"-p", "say hi"},
	},
	{
		name: "print-tool-loop",
		args: []string{"-p", `tool:Bash {"command":"echo diffprobe"}`, "--permission-mode", "bypassPermissions"},
	},
	{
		name: "print-stream-json",
		args: []string{"-p", "say hi", "--output-format", "stream-json", "--verbose"},
	},
	{
		name:        "interactive-single-turn",
		interactive: true,
		oauthOnly:   true,
		send:        "say hi\r",
		waitFor:     "Mock",
	},
}

// TestDiffScenarios runs every scenario through both binaries and prints
// a field-level comparison table. Diffs are surfaced, not failed on —
// the harness fails only when a side can't run the scenario at all.
func TestDiffScenarios(t *testing.T) {
	for _, sc := range diffScenarios {
		t.Run(sc.name, func(t *testing.T) { runDiffScenario(t, sc) })
	}
}

// TestDiffPrintSayHi runs just the baseline scenario — the quickest
// signal that both sides are wired to the mock correctly.
func TestDiffPrintSayHi(t *testing.T) {
	runDiffScenario(t, diffScenarios[0])
}
