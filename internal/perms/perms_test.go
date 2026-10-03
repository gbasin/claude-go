package perms

import (
	"encoding/json"
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
	if Check(ModePlan, nil, "Write", j(`{"file_path":"/tmp/x"}`), "/tmp") != Deny {
		t.Error("plan mode should deny Write")
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
