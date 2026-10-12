package repl

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gbasin/claude-go/internal/agent"
	"github.com/gbasin/claude-go/internal/tools"
)

func permTestLoop(mode string) *agent.Loop {
	return &agent.Loop{
		PermMode:  mode,
		PermRules: map[string][]string{},
		ToolCtx:   &tools.Context{CWD: "/tmp/proj"},
	}
}

func TestPermOptsBashShape(t *testing.T) {
	l := permTestLoop("default")
	p := &promptUI{}
	opts, inputs := p.permOpts(l, "Bash",
		json.RawMessage(`{"command":"echo hi"}`), "echo hi", true)
	if len(opts) != 4 {
		t.Fatalf("Bash dialog should have 4 rows (yes/prefix/auto/no), got %d: %+v", len(opts), opts)
	}
	if opts[0].label != "Yes" || opts[0].input != "feedback" ||
		opts[0].placeholder != "and tell Claude what to do next" {
		t.Fatalf("row 1 wrong: %+v", opts[0])
	}
	if opts[1].input != "prefix" ||
		!strings.Contains(opts[1].label, "don’t ask again for") ||
		opts[1].placeholder != "command prefix (e.g., npm run *)" ||
		!strings.Contains(opts[1].suffix, " in ") {
		t.Fatalf("prefix row wrong: %+v", opts[1])
	}
	// the editable prefix is seeded with the suggested rule content
	if got := string(inputs[1]); got != "echo:*" {
		t.Fatalf("prefix seed = %q, want %q", got, "echo:*")
	}
	if opts[2].action != "automode" ||
		!strings.Contains(opts[2].label, "switch to auto mode") {
		t.Fatalf("auto-mode row wrong: %+v", opts[2])
	}
	if opts[3].label != "No" || opts[3].input != "feedback" ||
		opts[3].placeholder != "and tell Claude what to do differently" {
		t.Fatalf("deny row wrong: %+v", opts[3])
	}
}

func TestPermOptsNoAutoRowWhenSuppressed(t *testing.T) {
	// real hides the enable-auto row when the ask was forced by a rule or
	// auto mode is already active — offerAuto=false mirrors that gate
	l := permTestLoop("default")
	p := &promptUI{}
	opts, _ := p.permOpts(l, "Bash",
		json.RawMessage(`{"command":"echo hi"}`), "echo hi", false)
	for _, o := range opts {
		if o.action == "automode" {
			t.Fatalf("auto-mode row should be suppressed: %+v", opts)
		}
	}
}

func TestPermOptsFileToolShape(t *testing.T) {
	l := permTestLoop("default")
	p := &promptUI{}
	opts, _ := p.permOpts(l, "Write",
		json.RawMessage(`{"file_path":"/tmp/proj/x.txt","content":"a"}`), "x.txt", false)
	if len(opts) != 3 {
		t.Fatalf("file-tool dialog should have 3 rows, got %d: %+v", len(opts), opts)
	}
	mid := opts[1]
	if mid.action != "acceptedits" ||
		!strings.Contains(mid.label, "accept edits (auto-approve file edits and common file commands)") ||
		!strings.Contains(mid.label, "(shift+tab)") {
		t.Fatalf("session row wrong: %+v", mid)
	}
	if strings.ContainsAny(mid.label, "\n\r") {
		t.Fatalf("session row must be single-line: %q", mid.label)
	}
}

func TestPermOptsExitPlan(t *testing.T) {
	l := permTestLoop("plan")
	p := &promptUI{}
	opts, _ := p.permOpts(l, "ExitPlanMode", nil, "", false)
	var labels []string
	for _, o := range opts {
		labels = append(labels, o.label)
	}
	joined := strings.Join(labels, " | ")
	// keep-context lanes: resume-auto (default install) then the default
	// lane, then the deny input row — no clear-context row by default
	if !strings.Contains(joined, "Yes, and use auto mode") {
		t.Fatalf("missing resume-auto lane: %s", joined)
	}
	if !strings.Contains(joined, "Yes, manually approve edits") {
		t.Fatalf("missing default lane: %s", joined)
	}
	last := opts[len(opts)-1]
	if last.label != "No, keep planning" || last.input != "feedback" ||
		last.placeholder != "Tell Claude what to change" {
		t.Fatalf("deny row wrong: %+v", last)
	}
	for _, o := range opts {
		if strings.Contains(o.label, "clear context") {
			t.Fatalf("clear-context row should be opt-in via settings: %s", joined)
		}
	}
}

func TestPermOptsExitPlanBypassLane(t *testing.T) {
	t.Setenv("CLAUDE_GO_ALLOW_BYPASS_PERMISSIONS", "1")
	l := permTestLoop("plan")
	p := &promptUI{}
	opts, _ := p.permOpts(l, "ExitPlanMode", nil, "", false)
	if len(opts) < 3 ||
		!strings.Contains(opts[0].label, "BYPASS PERMISSIONS (no further prompts)") {
		t.Fatalf("bypass lane should lead the keep-context rows: %+v", opts)
	}
	if opts[0].mode != "bypassPermissions" {
		t.Fatalf("bypass lane should carry mode bypassPermissions: %+v", opts[0])
	}
}

func TestUnrenderableParam(t *testing.T) {
	// depth ≥ 64 → sentinel
	deep := map[string]any{}
	cur := deep
	for i := 0; i < 70; i++ {
		nxt := map[string]any{}
		cur["k"] = nxt
		cur = nxt
	}
	if !unrenderableParam(map[string]any{"v": deep}) {
		t.Fatal("64+ deep value should be unrenderable")
	}
	if !unrenderableParam(map[string]any{"v": strings.Repeat("x", 200_001)}) {
		t.Fatal(">200k char value should be unrenderable")
	}
	if unrenderableParam(map[string]any{"command": "ls", "timeout": 30}) {
		t.Fatal("ordinary params should render")
	}
	// the dialog content line degrades to the guard row
	lines := permContentLines("SomeTool",
		json.RawMessage(`{"v":"`+strings.Repeat("x", 200_001)+`"}`), "x")
	if len(lines) != 1 || !strings.Contains(lines[0],
		"value too large or too deeply nested to render") {
		t.Fatalf("expected guard row, got %v", lines)
	}
}
