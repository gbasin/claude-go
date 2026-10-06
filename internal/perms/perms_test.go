package perms

import (
	"claude-go/internal/config"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func j(s string) json.RawMessage { return json.RawMessage(s) }

func TestBashRules(t *testing.T) {
	r := &Rules{Allow: []string{"Bash(ls:*)", "Bash(git status:*)", "Bash(npm run build)"}}
	cases := []struct {
		cmd  string
		want Decision
	}{
		{"ls -la", Allow},
		{"ls", Allow},
		{"git status --short", Allow},
		{"npm run build", Allow},
		{"npm run build --watch", Deny - Deny + Ask}, // not covered -> Ask in default
		{"rm -rf /", Ask},
	}
	for _, c := range cases {
		got := Check(ModeDefault, r, "Bash", j(`{"command":"`+c.cmd+`"}`), "/tmp")
		if got != c.want {
			t.Errorf("Bash(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestModes(t *testing.T) {
	if Check(ModeBypass, nil, "Bash", j(`{"command":"rm -rf /"}`), "/tmp") != Allow {
		t.Error("bypass should allow everything")
	}
	// useAutoModeDuringPlan (default true) routes non-read-only plan-mode
	// calls to the auto-eval lane → Ask, not blanket Deny
	if Check(ModePlan, nil, "Write", j(`{"file_path":"/tmp/x"}`), "/tmp") != Ask {
		t.Error("plan mode should route Write to the auto-eval (Ask)")
	}
	// explicit off → hard deny like before
	t.Setenv("HOME", t.TempDir())
	writePlanAutoOff(t)
	if Check(ModePlan, nil, "Write", j(`{"file_path":"/tmp/x"}`), "/tmp") != Deny {
		t.Error("useAutoModeDuringPlan:false should deny Write in plan mode")
	}
	if Check(ModePlan, nil, "Read", j(`{"file_path":"/tmp/x"}`), "/tmp") != Allow {
		t.Error("plan mode should allow Read")
	}
	if Check(ModeAcceptEdit, nil, "Edit", j(`{"file_path":"/tmp/x","old_string":"a","new_string":"b"}`), "/tmp") != Allow {
		t.Error("acceptEdits should allow Edit")
	}
	if Check(ModeAuto, nil, "Read", j(`{"file_path":"/tmp/x"}`), "/tmp") != Allow {
		t.Error("auto should allow read-only")
	}
}

func TestFileGlob(t *testing.T) {
	r := &Rules{Allow: []string{"Read(/tmp/**)"}}
	if Check(ModeDefault, r, "Read", j(`{"file_path":"/tmp/a/b/c.txt"}`), "/tmp") != Allow {
		t.Error("expected /tmp/** to match nested path")
	}
	r2 := &Rules{Deny: []string{"Read(/etc/**)"}}
	if Check(ModeDefault, r2, "Read", j(`{"file_path":"/etc/passwd"}`), "/tmp") != Deny {
		t.Error("expected deny rule to win")
	}
}

func TestOrderAskBeatsAllow(t *testing.T) {
	r := &Rules{Allow: []string{"Bash(ls:*)"}, Ask: []string{"Bash(ls:*)"}, Deny: []string{}}
	if d := Check(ModeDefault, r, "Bash", bashIn("ls -la"), "/tmp"); d != Ask {
		t.Errorf("ask rule should beat allow rule, got %v", d)
	}
}

func TestSubcommandSplit(t *testing.T) {
	r := &Rules{Allow: []string{"Bash(ls:*)"}, Deny: []string{"Bash(rm:*)"}}
	// ls allowed but rm inside the chain denied
	if d := Check(ModeDefault, r, "Bash", bashIn("ls && rm -rf /"), "/tmp"); d != Deny {
		t.Errorf("rm in chain should deny, got %v", d)
	}
	// all allowed -> allow
	r2 := &Rules{Allow: []string{"Bash(ls:*)", "Bash(cat:*)"}}
	if d := Check(ModeDefault, r2, "Bash", bashIn("ls | cat"), "/tmp"); d != Allow {
		t.Errorf("allowed chain got %v", d)
	}
}

func TestDontAsk(t *testing.T) {
	if d := Check(ModeDontAsk, nil, "Bash", bashIn("rm -rf /"), "/tmp"); d != Deny {
		t.Errorf("dontAsk should deny, got %v", d)
	}
}

func TestMcpServerRule(t *testing.T) {
	r := &Rules{Allow: []string{"mcp__deepwiki"}}
	if d := Check(ModeDefault, r, "mcp__deepwiki__search", nil, "/tmp"); d != Allow {
		t.Errorf("server-level rule should match all tools, got %v", d)
	}
}

func bashIn(cmd string) []byte {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

func writePlanAutoOff(t *testing.T) {
	t.Helper()
	dir := config.ClaudeDir()
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"useAutoModeDuringPlan":false}`), 0o644)
}
