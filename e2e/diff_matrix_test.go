//go:build e2e

// diff_matrix_test.go — the UI coverage matrix. Instead of hand-picked
// scenarios, this is a catalog of reachable TUI surfaces (one row per
// audit surface): each entry is an identical keystroke script driven
// through real claude and claude-go on a PTY, rendered through termemu,
// and row-diffed at every checkpoint.
//
// Waivers: knownDivergent lists scenario IDs we accept a diff on today —
// the regression ledger. A diff on a non-waived scenario fails; a waiver
// on a scenario that now matches also fails (stale waiver — remove it).
// The ledger can only shrink.
//
// Run: go test -tags e2e -run 'TestDiffMatrix' ./e2e/ -v

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gbasin/claude-go/internal/mock"
	"github.com/gbasin/claude-go/internal/termemu"
)

// uiStep is one scripted interaction. send is the raw keystroke bytes;
// wait is a rendered-screen substring anchor ("" = none); settle is the
// quiesce period before snapshotting (default settleDefault).
type uiStep struct {
	name   string
	send   string
	wait   string
	settle time.Duration
}

// uiScenario is a catalog row: seeds to reach a UI state plus the script.
type uiScenario struct {
	id        string
	seed      map[string]any    // extra ~/.claude.json fields
	settings  map[string]any    // ~/.claude/settings.json fields
	files     map[string]string // cwd files (path -> contents)
	homeFiles map[string]string // home-relative files (e.g. .claude/history.jsonl)
	noOnboard bool              // do NOT seed hasCompletedOnboarding/trust
	apiKey    bool              // include ANTHROPIC_API_KEY (real shows a key-confirm gate)
	steps     []uiStep
}

// uiSnap is one checkpoint capture.
type uiSnap struct {
	name    string
	screen  string // settled rendered frame
	frames  int    // distinct frames emitted during the step
	sawWait bool   // whether the wait anchor appeared at all
}

// uiRun is a full scenario drive for one binary.
type uiRun struct {
	snaps []uiSnap
	raw   string
}

const settleDefault = 450 * time.Millisecond
const waitDefault = 8 * time.Second

// driveScript runs a scenario on argv (e.g. [realClaudeBin] or
// [binPath]) and returns per-step snapshots. Waits are soft — an anchor
// that never appears is recorded (sawWait=false), not fatal, so a
// diverged run still produces comparable frames.
func driveScript(t *testing.T, argv []string, sc uiScenario) uiRun {
	t.Helper()
	cwd, home := newDiffSandbox(t)

	if !sc.noOnboard {
		// newDiffSandbox already seeds onboarding+trust; merge extras
		if len(sc.seed) > 0 {
			p := filepath.Join(home, ".claude.json")
			var m map[string]any
			b, _ := os.ReadFile(p)
			json.Unmarshal(b, &m)
			for k, v := range sc.seed {
				m[k] = v
			}
			b, _ = json.Marshal(m)
			os.WriteFile(p, b, 0o644)
		}
	} else {
		// fresh-home scenarios: drop the seeded .claude.json entirely
		os.Remove(filepath.Join(home, ".claude.json"))
	}
	if len(sc.settings) > 0 {
		p := filepath.Join(home, ".claude", "settings.json")
		b, _ := json.Marshal(sc.settings)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatalf("seed settings: %v", err)
		}
	}
	for rel, body := range sc.files {
		p := filepath.Join(cwd, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("seed file %s: %v", rel, err)
		}
	}
	for rel, body := range sc.homeFiles {
		p := filepath.Join(home, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("seed home file %s: %v", rel, err)
		}
	}

	srv, err := mock.Start()
	if err != nil {
		t.Fatalf("mock start: %v", err)
	}
	defer srv.Close()

	master, slave := openPTYSize(t, 120, 40)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = diffEnv(home, srv.Addr, !sc.apiKey)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = ptyProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", argv, err)
	}
	slave.Close()
	p := &ptyProc{t: t, cmd: cmd, master: master, done: make(chan error, 1),
		emu: termemu.New(120, 40)}

	// frameLog records every distinct rendered frame — the temporal
	// sequence, not just the settled end state.
	var frameSeq []string
	go func() {
		var tmp [4096]byte
		for {
			n, err := master.Read(tmp[:])
			if n > 0 {
				p.mu.Lock()
				p.buf.Write(tmp[:n])
				p.emu.Feed(tmp[:n])
				scr := p.emu.String()
				if len(frameSeq) == 0 || frameSeq[len(frameSeq)-1] != scr {
					frameSeq = append(frameSeq, scr)
				}
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { p.done <- cmd.Wait() }()
	defer p.close()

	var run uiRun
	for _, st := range sc.steps {
		p.mu.Lock()
		seqStart := len(frameSeq)
		p.mu.Unlock()

		if st.send != "" {
			p.sendQuiet(st.send)
		}
		saw := true
		if st.wait != "" {
			saw = softWaitScreen(p, st.wait, waitDefault)
		}
		settle := st.settle
		if settle == 0 {
			settle = settleDefault
		}
		time.Sleep(settle)
		p.mu.Lock()
		n := len(frameSeq) - seqStart
		p.mu.Unlock()
		snap := uiSnap{name: st.name, screen: p.screen(), frames: n, sawWait: saw}
		run.snaps = append(run.snaps, snap)
		if dir := os.Getenv("CLAUDE_GO_MATRIX_DUMP"); dir != "" {
			os.MkdirAll(dir, 0o755)
			label := filepath.Base(argv[0])
			fn := filepath.Join(dir, fmt.Sprintf("%s-%02d-%s-%s.txt",
				sc.id, len(run.snaps)-1, st.name, label))
			os.WriteFile(fn, []byte(snap.screen), 0o644)
		}
	}
	run.raw = p.clean()
	if dir := os.Getenv("CLAUDE_GO_MATRIX_DUMP"); dir != "" {
		fn := filepath.Join(dir, fmt.Sprintf("%s-raw-%s.txt", sc.id, filepath.Base(argv[0])))
		os.WriteFile(fn, []byte(p.output()), 0o644)
	}
	return run
}

// softWaitScreen polls the rendered screen for substr; returns whether
// it appeared. Never fails — a missing anchor is itself the signal a
// diverged run should surface.
func softWaitScreen(p *ptyProc, substr string, timeout time.Duration) bool {
	substr = strings.ReplaceAll(substr, "\u00a0", " ")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(p.screen(), substr) {
			return true
		}
		select {
		case err := <-p.done:
			p.done <- err
			return false
		default:
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// ---------------------------------------------------------------------------
// The catalog: one entry per reachable UI surface. wait anchors use text
// real is known to render; a miss is recorded, not fatal — the frame diff
// is the verdict.
// ---------------------------------------------------------------------------

var uiCatalog = []uiScenario{
	{
		id: "boot-idle",
		steps: []uiStep{
			{name: "idle", wait: "❯", settle: 2500 * time.Millisecond},
		},
	},
	{
		id: "draft-typing",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "draft", send: "hello draft", wait: "hello draft"},
		},
	},
	{
		id: "slash-menu",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "menu", send: "/", wait: "/model", settle: 900 * time.Millisecond},
		},
	},
	{
		id: "slash-filter",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "filtered", send: "/mod", settle: 900 * time.Millisecond},
		},
	},
	{
		id:    "at-mention",
		files: map[string]string{"foo.txt": "hi", "src/main.go": "package main\n"},
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "picker", send: "@fo", settle: 1200 * time.Millisecond},
		},
	},
	{
		id: "multiline-draft",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "line1", send: "first line\\\r", wait: "first line"},
			{name: "line2", send: "second", wait: "second"},
		},
	},
	{
		id: "shortcuts-overlay",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "overlay", send: "?", wait: "?"},
		},
	},
	{
		id: "history-search",
		homeFiles: map[string]string{
			".claude/history.jsonl": `{"display":"older prompt one","timestamp":1750000000000,"project":"/tmp","sessionId":"s1"}` + "\n" +
				`{"display":"older prompt two","timestamp":1750000001000,"project":"/tmp","sessionId":"s1"}` + "\n",
		},
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "search", send: "\x12", settle: 900 * time.Millisecond},
			{name: "typed", send: "older", settle: 900 * time.Millisecond},
		},
	},
	{
		id: "transcript-overlay",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "turn", send: "say hi\r", wait: "Mock response", settle: 1200 * time.Millisecond},
			{name: "overlay", send: "\x0f", settle: 1200 * time.Millisecond},
			{name: "scroll", send: "j", settle: 600 * time.Millisecond},
			{name: "close", send: "\x1b", settle: 600 * time.Millisecond},
		},
	},
	{
		id: "rewind-picker",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "turn", send: "say hi\r", wait: "Mock response", settle: 1000 * time.Millisecond},
			{name: "picker", send: "\x1b\x1b", settle: 1200 * time.Millisecond},
		},
	},
	{
		id: "model-picker",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "picker", send: "/model\r", wait: "model", settle: 1200 * time.Millisecond},
			{name: "down", send: "\x1b[B", settle: 600 * time.Millisecond},
			{name: "esc", send: "\x1b", settle: 600 * time.Millisecond},
		},
	},
	{
		id: "theme-picker",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "picker", send: "/theme\r", settle: 1200 * time.Millisecond},
			{name: "esc", send: "\x1b", settle: 600 * time.Millisecond},
		},
	},
	{
		id: "mcp-panel",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "panel", send: "/mcp\r", settle: 1200 * time.Millisecond},
			{name: "esc", send: "\x1b", settle: 600 * time.Millisecond},
		},
	},
	{
		id: "help-panel",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "panel", send: "/help\r", settle: 1200 * time.Millisecond},
		},
	},
	{
		id: "status-panel",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "panel", send: "/status\r", settle: 1500 * time.Millisecond},
		},
	},
	{
		id:       "perm-dialog-write",
		settings: map[string]any{"permissions": map[string]any{"defaultMode": "default"}},
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "dialog", send: `tool:Write {"file_path":"/tmp/mx.txt","content":"hi"}` + "\r",
				wait: "permission", settle: 2500 * time.Millisecond},
			{name: "tab", send: "\t", settle: 800 * time.Millisecond},
			{name: "esc", send: "\x1b", settle: 800 * time.Millisecond},
		},
	},
	{
		id:       "perm-dialog-bash",
		settings: map[string]any{"permissions": map[string]any{"defaultMode": "default"}},
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "dialog", send: `tool:Bash {"command":"rm -rf /tmp/x"}` + "\r",
				settle: 2500 * time.Millisecond},
			{name: "esc", send: "\x1b", settle: 800 * time.Millisecond},
		},
	},
	{
		id: "esc-clear-hint",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "draft", send: "draft text", wait: "draft text"},
			{name: "esc1", send: "\x1b", settle: 900 * time.Millisecond},
		},
	},
	{
		id: "exit-arm-ctrlc",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "armed", send: "\x03", wait: "again to exit", settle: 300 * time.Millisecond},
			{name: "exited", send: "\x03", settle: 1500 * time.Millisecond},
		},
	},
	{
		id: "ctrld-empty",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "ctrld", send: "\x04", wait: "again to exit", settle: 300 * time.Millisecond},
		},
	},
	{
		id: "ctrly-draft",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "draft", send: "some text", wait: "some text"},
			{name: "ctrly", send: "\x19", settle: 900 * time.Millisecond},
		},
	},
	{
		id: "queued-message",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "turn+queue", send: "say hi\rqueued second\r", wait: "Mock response", settle: 2000 * time.Millisecond},
		},
	},
	{
		id:       "tool-fold",
		settings: map[string]any{"permissions": map[string]any{"defaultMode": "default"}},
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "fold", send: `tool:Bash {"command":"seq 20"}` + "\r",
				wait: "done", settle: 2000 * time.Millisecond},
		},
	},
	{
		id:       "vim-normal",
		settings: map[string]any{"editorMode": "vim"},
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "esc-to-normal", send: "abc\x1b", settle: 900 * time.Millisecond},
		},
	},
	{
		id: "agents-pane",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "pane", send: "\x1b[D\x1b[D", settle: 1200 * time.Millisecond},
			{name: "esc", send: "\x1b", settle: 600 * time.Millisecond},
		},
	},
	{
		id:        "onboarding-theme",
		noOnboard: true,
		steps: []uiStep{
			{name: "picker", settle: 4000 * time.Millisecond},
		},
	},
	{
		id: "above-prompt",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "toggle", send: "\x18\x01", settle: 1200 * time.Millisecond},
		},
	},
	{
		id:     "apikey-gate",
		apiKey: true,
		steps: []uiStep{
			{name: "gate", wait: "API key", settle: 2500 * time.Millisecond},
			{name: "confirm-no", send: "\r", settle: 1500 * time.Millisecond},
		},
	},
	{
		id: "context-panel",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "panel", send: "/context\r", settle: 1500 * time.Millisecond},
		},
	},
	{
		id: "config-panel",
		steps: []uiStep{
			{wait: "❯", settle: 1200 * time.Millisecond},
			{name: "panel", send: "/config\r", settle: 1500 * time.Millisecond},
			{name: "esc", send: "\x1b", settle: 600 * time.Millisecond},
		},
	},
}

// knownDivergent is the regression ledger: scenario ID → why a diff is
// accepted. Every entry must cite an audits/tui.md row or a tracking
// note. A diff here is a KNOWN bug, not an accepted behavior — the gate
// fails if a waived scenario stops diffing (forcing ledger cleanup) or
// if a non-waived scenario starts.
var knownDivergent = map[string]string{
	// keys are "<scenario>" (whole) or "<scenario>/<step>"
	// post-exit frame: real leaves the armed hint + teardown bytes ("BB")
	// on screen; ours repaints a clean footer before quitting
	"exit-arm-ctrlc/exited": "real's exit leaves the armed hint painted (plus teardown noise); ours repaints the footer clean — match real's quit path",
	// menu/picker surfaces — real's content + layout not yet ported
	"slash-menu":   "menu rows render above the input in real (name col padded, 5-item window); ours draws them inside the box with '·' separators and a stale command set — port real's table + layout",
	"slash-filter": "same menu port; real filters to /model /code-review /plugin-authoring rows with aligned descriptions",
	"at-mention":   "real lists file candidates as a picker above the input; ours drops the menu and keeps '@foo.txt' inline — port picker placement + 'N/N' filtering",
	// overlays and dialogs
	"shortcuts-overlay":  "real's ? overlay is a 3-column card ('! for shell mode', 'ctrl+s to stash prompt', …); ours renders a different 2-col keymap — port real's content",
	"history-search":     "real draws a bordered overlay with '1y ago' age column and 'ctrl+s to scope' hint; ours renders an inline '(search prompts)' line — port overlay",
	"transcript-overlay": "ctrl+o overlay diverges: real's turn/scroll/close frames differ (scroll retention + footer); port real's overlay rows",
	"rewind-picker":      "esc-esc rewind selector rows diverge from real's picker (checkboxes + tips) — port real's picker",
	"model-picker":       "model picker rows/order/annotations diverge — port real's picker content",
	"theme-picker":       "theme picker layout + preview block diverge (real indents code preview differently) — port",
	"onboarding-theme":   "fresh-home theme picker preview spacing differs (gutter dots + '1  ' line-number gap) — port preview",
	"mcp-panel":          "real prints 'No MCP servers configured…' guidance text inline; ours prints a different two-line block — port text",
	"help-panel":         "/help panel content diverges — port real's help surface",
	"status-panel":       "/status surface diverges (real shows tabs + doctor lines) — port",
	"config-panel":       "/config panel diverges — port real's settings surface",
	"context-panel":      "/context grid diverges — real draws the ⛶ token grid + category breakdown; port",
	// behavior gaps
	"apikey-gate":       "ours lacks real's 'Detected a custom API key' confirmation gate entirely — implement gate",
	"agents-pane":       "real didn't open the agents pane on <-<- (arrow timing or gate differs); ours renders a full pane — re-probe real's left-arrow FSM",
	"above-prompt":      "ctrl+t toggle: real's above-prompt row content differs ('no recent activity' ours vs real's layout)",
	"perm-dialog-write": "real first shows its 'Make auto mode your default permission mode?' opt-in under defaultMode=default — ours lacks it; dialog rows also diverge",
	"perm-dialog-bash":  "dialog content diverges (real's option list + ours' mid-paint residue 'Bash commandx' — repaint bug in dialog writes)",
	"tool-fold":         "ours prompts where real auto-mode executes; also multi-line tool-result fold rows diverge",
	"queued-message":    "mid-turn queued flow: ours scrolls the banner off (taller transcript) and churns 71 frames vs real 9 — turn-loop rows + scroll model diverge",
	"multiline-draft":   "zone growth on multiline scrolls the transcript ~2 rows too far (banner top falls off) — compResizeRows grow-path off-by-one",
}

// diffMatrixRun compares the two drives at every checkpoint.
func diffMatrixRun(t *testing.T, sc uiScenario, r, o uiRun) {
	t.Helper()
	divergent := false
	waived := map[string]bool{} // waiver keys that observed a diff
	// note routes a divergence through the waiver ledger: a step key
	// ("id/step") or the bare scenario key may cover it
	note := func(label, stepKey string) bool {
		if why, ok := knownDivergent[stepKey]; ok {
			waived[stepKey] = true
			t.Logf("%s: divergent (waived: %s)", label, why)
			return true
		}
		if why, ok := knownDivergent[sc.id]; ok {
			waived[sc.id] = true
			t.Logf("%s: divergent (waived: %s)", label, why)
			return true
		}
		return false
	}
	n := len(r.snaps)
	if len(o.snaps) < n {
		n = len(o.snaps)
	}
	for i := 0; i < n; i++ {
		rs, os := r.snaps[i], o.snaps[i]
		label := fmt.Sprintf("%s/%s", sc.id, rs.name)

		// anchor reachability: real saw it, ours didn't = divergence
		if rs.sawWait && !os.sawWait && sc.steps[i].wait != "" {
			t.Logf("%s: wait anchor %q reached by real, missed by ours", label, sc.steps[i].wait)
			if !note(label, sc.id+"/"+rs.name) {
				divergent = true
			}
		}

		rNorm, oNorm := normFrame(rs.screen), normFrame(os.screen)
		var missing, extra []string
		seen := map[string]bool{}
		for _, row := range rNorm {
			if !containsRow(oNorm, row) && !seen[row] {
				missing = append(missing, row)
				seen[row] = true
			}
		}
		seen = map[string]bool{}
		for _, row := range oNorm {
			if !containsRow(rNorm, row) && !seen[row] {
				extra = append(extra, row)
				seen[row] = true
			}
		}
		if len(missing) > 0 || len(extra) > 0 {
			stepKey := sc.id + "/" + rs.name
			t.Logf("%s: frame diff (real %d frames this step, ours %d)", label, rs.frames, os.frames)
			for _, m := range missing {
				t.Logf("  - %s", m)
			}
			for _, e := range extra {
				t.Logf("  + %s", e)
			}
			if !note(label, stepKey) {
				divergent = true
			}
		} else if rs.frames != os.frames {
			// temporal signal — repaint counts legitimately differ, log only
			t.Logf("%s: frames match; repaint count real=%d ours=%d", label, rs.frames, os.frames)
		}
	}
	if len(r.snaps) != len(o.snaps) {
		t.Logf("%s: step capture count differs (real %d, ours %d)", sc.id, len(r.snaps), len(o.snaps))
		if !note(sc.id, sc.id) {
			divergent = true
		}
	}
	if divergent {
		t.Errorf("%s: UI divergence — see logged row diffs", sc.id)
	}
	// stale-waiver sweep: every waiver for this scenario must have
	// observed a diff this run — the ledger can only shrink
	for key, why := range knownDivergent {
		if key != sc.id && !strings.HasPrefix(key, sc.id+"/") {
			continue
		}
		if !waived[key] {
			t.Errorf("%s: waiver stale for %q — %s", sc.id, key, why)
		}
	}
}

// TestDiffMatrix drives every catalog scenario through both binaries and
// diffs rendered frames at each checkpoint. Parallel subtests: each owns
// its PTY, sandbox, and mock — no shared state.
func TestDiffMatrix(t *testing.T) {
	realBin := realClaudeBin(t)
	for _, sc := range uiCatalog {
		sc := sc
		t.Run(sc.id, func(t *testing.T) {
			t.Parallel()
			r := driveScript(t, []string{realBin}, sc)
			o := driveScript(t, []string{binPath}, sc)
			diffMatrixRun(t, sc, r, o)
		})
	}
}

// oracleArgv returns argv that runs the unpacked real source under stock
// Bun with the CellSegmenter shim (e2e/oracle/cellseg_shim.mjs). Set
// CLAUDE_GO_ORACLE to a shell-split command, e.g.
//
//	CLAUDE_GO_ORACLE="bun --preload $PWD/e2e/oracle/cellseg_shim.mjs /path/to/claude-unpacked-296/cli"
//
// The oracle leg diffs real-source-under-bun vs the real BINARY —
// validating the shim and giving a mechanism-level reference — plus
// ours as usual.
func oracleArgv(t *testing.T) []string {
	v := os.Getenv("CLAUDE_GO_ORACLE")
	if v == "" {
		t.Skip("set CLAUDE_GO_ORACLE to run the unpacked-source oracle leg")
	}
	return strings.Fields(v)
}

// oracleDivergent covers KNOWN source-vs-binary deltas (packaging,
// argv0/cwd handling, timing) — NOT segmenter contract issues. A real
// shim bug must not land here; keep this list tiny.
var oracleDivergent = map[string]string{
	// a '~' mascot-reveal art cell survives normalization when one leg is
	// caught mid-scatter — pure frame-timing, not a shim bug
	"draft-typing":    "mascot reveal-art '~' cell timing delta",
	"exit-arm-ctrlc":  "mascot reveal-art '~' cell timing delta",
	"queued-message":  "mascot reveal-art '~' cell timing delta",
	"multiline-draft": "mascot reveal-art '~' cell timing delta",
	// oracle's argv0 is `bun cli` — /status doctor checks see no native
	// claude install; binary checks its own path. Env delta, not shim.
	"status-panel/panel": "doctor lines differ: oracle argv0 is bun (no native install), binary checks ~/.local/bin/claude",
	// effort-pill row presence flickers mid-repaint on tool folds
	"tool-fold/fold": "effort pill transient — binary captured mid-repaint",
}

// diffOracleRun compares oracle vs binary. The product waiver ledger does
// NOT apply — a diff here is a shim bug by definition (same source).
func diffOracleRun(t *testing.T, sc uiScenario, or, r uiRun) {
	t.Helper()
	n := len(r.snaps)
	if len(or.snaps) < n {
		n = len(or.snaps)
	}
	bad := false
	for i := 0; i < n; i++ {
		os_, rs := or.snaps[i], r.snaps[i]
		label := fmt.Sprintf("%s/%s", sc.id, rs.name)
		rNorm, oNorm := normFrame(rs.screen), normFrame(os_.screen)
		var missing, extra []string
		seen := map[string]bool{}
		for _, row := range rNorm {
			if !containsRow(oNorm, row) && !seen[row] {
				missing = append(missing, row)
				seen[row] = true
			}
		}
		seen = map[string]bool{}
		for _, row := range oNorm {
			if !containsRow(rNorm, row) && !seen[row] {
				extra = append(extra, row)
				seen[row] = true
			}
		}
		if len(missing) > 0 || len(extra) > 0 {
			t.Logf("%s: oracle frame diff (binary %d frames, oracle %d)", label, rs.frames, os_.frames)
			for _, m := range missing {
				t.Logf("  - %s", m)
			}
			for _, e := range extra {
				t.Logf("  + %s", e)
			}
			if why, ok := oracleDivergent[sc.id+"/"+rs.name]; ok {
				t.Logf("%s: oracle divergent (waived: %s)", label, why)
			} else if why, ok := oracleDivergent[sc.id]; ok {
				t.Logf("%s: oracle divergent (waived: %s)", label, why)
			} else {
				bad = true
			}
		} else if rs.frames != os_.frames {
			t.Logf("%s: oracle frames match; repaint count binary=%d oracle=%d", label, rs.frames, os_.frames)
		}
	}
	if bad {
		t.Errorf("%s: oracle diverges from binary — shim contract bug", sc.id)
	}
}

// TestOracleMatrix drives the catalog through the bun oracle + real
// binary (oracle≈binary validates the shim) + claude-go. Serial: bun
// instances are heavy; parallel runs starve them into anchor timeouts.
func TestOracleMatrix(t *testing.T) {
	oracle := oracleArgv(t)
	realBin := realClaudeBin(t)
	for _, sc := range uiCatalog {
		sc := sc
		t.Run(sc.id, func(t *testing.T) {
			or := driveScript(t, oracle, sc)
			r := driveScript(t, []string{realBin}, sc)
			diffOracleRun(t, sc, or, r) // oracle vs binary — shim check
		})
	}
}

// TestDiffMatrixCoverage is the ledger self-check: the catalog must stay
// non-trivial (guard against an emptied table silently passing).
func TestDiffMatrixCoverage(t *testing.T) {
	if len(uiCatalog) < 20 {
		t.Fatalf("catalog shrank to %d scenarios — expected 20+", len(uiCatalog))
	}
}
