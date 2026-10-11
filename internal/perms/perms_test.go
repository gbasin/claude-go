package perms

import (
	"encoding/json"
	"github.com/gbasin/claude-go/internal/config"
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

// Real's rule semantics (mod_0056.js parsePermissionRule).
func TestRuleCorners(t *testing.T) {
	r := &Rules{Allow: []string{
		"Bash()",              // exact-empty — matches NOTHING
		"Bash(git push *)",    // trailing " *" — also matches bare `git push`
		"Bash(git * --force)", // interior wildcard
		"Bash(ls:*)",          // prefix rule
		"mcp__github",         // bare server — all its tools
		"mcp__sentry__list",   // narrowed to one tool
		"WebFetch(domain:example.com)",
		"Edit(src/**)", // matches src itself + descendants
	}}
	chk := func(tool, spec string) Decision { return Check(ModeDefault, r, tool, mkInput(tool, spec), "/repo") }

	if chk("Bash", "rm -rf /") != Deny && chk("Bash", "rm -rf /") != Ask {
		// not denied/asked — allowed by some rule? shouldn't be
	}
	// Bash() must NOT allow anything
	if chk("Bash", "anything") == Allow {
		t.Error("Bash() must match nothing")
	}
	if chk("Bash", "git push") != Allow {
		t.Error("git push should match 'git push *'")
	}
	if chk("Bash", "git push origin main") != Allow {
		t.Error("git push origin main should match")
	}
	if chk("Bash", "git pushx") == Allow {
		t.Error("'git push *' must not match pushx")
	}
	if chk("Bash", "git rebase --force") != Allow {
		t.Error("interior * should match 'git rebase --force'")
	}
	if chk("Bash", "ls") != Allow || chk("Bash", "ls -la") != Allow {
		t.Error("ls:* should match ls and ls -la")
	}
	if chk("mcp__github__get_issue", "{}") != Allow {
		t.Error("bare mcp__github should allow all github tools")
	}
	if chk("mcp__sentry__list", "{}") != Allow {
		t.Error("mcp__sentry__list should allow the list tool")
	}
	if chk("mcp__sentry__other", "{}") == Allow {
		t.Error("mcp__sentry__list must not allow other sentry tools")
	}
	// real rejects parens on MCP rules ("MCP rules do not support
	// patterns in parentheses") — they match nothing
	paren := &Rules{Allow: []string{"mcp__sentry(list)"}}
	if Check(ModeDefault, paren, "mcp__sentry__list", j(`{}`), "/repo") == Allow {
		t.Error("mcp__sentry(list) is an invalid rule and must not match")
	}
	if chk("WebFetch", "https://example.com/x") != Allow {
		t.Error("WebFetch(domain:) should match the host")
	}
	// allow rules are permissive — a non-matching allow doesn't deny;
	// the domain rule still gates when present in deny
	rd := &Rules{Deny: []string{"WebFetch(domain:evil.com)"}}
	if Check(ModeDefault, rd, "WebFetch", j(`{"url":"https://evil.com/x"}`), "/repo") != Deny {
		t.Error("deny WebFetch(domain:evil.com) should deny")
	}
	if Check(ModeDefault, rd, "WebFetch", j(`{"url":"https://good.com/"}`), "/repo") == Deny {
		t.Error("deny domain rule must not match other hosts")
	}
	if chk("WebFetch", "https://sub.example.com") != Allow {
		t.Error("domain rule should match subdomains")
	}
	if chk("Edit", "/repo/src") != Allow {
		t.Error("src/** should match src itself")
	}
	if chk("Edit", "/repo/src/a/b/c.go") != Allow {
		t.Error("src/** should match deep descendants")
	}
}

func mkInput(tool, spec string) json.RawMessage {
	switch tool {
	case "WebFetch":
		return j(`{"url":"` + spec + `"}`)
	default:
		return j(`{"command":"` + spec + `","file_path":"` + spec + `"}`)
	}
}

// Real's MCP wildcard validation (mod_0072.js Kxt): allow rules may glob
// only the tool position of mcp__<literal-server>__*; deny/ask rules may
// glob anywhere.
func TestMcpWildcards(t *testing.T) {
	in := j(`{}`)
	allow := &Rules{Allow: []string{"mcp__github__*"}}
	if Check(ModeDefault, allow, "mcp__github__get_issue", in, "/r") != Allow {
		t.Error("mcp__github__* should allow all github tools")
	}
	if Check(ModeDefault, allow, "mcp__sentry__x", in, "/r") == Allow {
		t.Error("mcp__github__* must not allow other servers")
	}
	// wildcard server name is NOT valid in an allow rule
	allowSrv := &Rules{Allow: []string{"mcp__*"}}
	if Check(ModeDefault, allowSrv, "mcp__github__x", in, "/r") == Allow {
		t.Error("mcp__* is rejected in allow rules (Kxt)")
	}
	// ...but deny/ask rules may glob anywhere
	deny := &Rules{Deny: []string{"mcp__*"}}
	if Check(ModeDefault, deny, "mcp__github__x", in, "/r") != Deny {
		t.Error("mcp__* deny rule should match every mcp tool")
	}
	askSrv := &Rules{Ask: []string{"mcp__*__delete*"}}
	if Check(ModeDefault, askSrv, "mcp__github__delete_repo", in, "/r") != Ask {
		t.Error("mcp__*__delete* ask rule should match")
	}
}

// Real's globstar: `/**/` collapses to `/(?:.*/)?` — matches zero or more
// dirs. A bare trailing `**` is just `.*`.
func TestBashGlobstar(t *testing.T) {
	r := &Rules{Allow: []string{"Bash(cat /a/**/b.txt)"}}
	ok := func(cmd string) bool {
		return Check(ModeDefault, r, "Bash", j(`{"command":"`+cmd+`"}`), "/r") == Allow
	}
	if !ok("cat /a/b.txt") {
		t.Error("a/**/b should match a/b (zero dirs)")
	}
	if !ok("cat /a/x/y/b.txt") {
		t.Error("a/**/b should match a/x/y/b")
	}
	if ok("cat /a/b.bin") {
		t.Error("a/**/b must not match other filenames")
	}
}

// trailing " *" becomes optional only when the pattern has exactly one
// unescaped star (real: S.endsWith(" .*") && b===1)
func TestBashTrailingStarCount(t *testing.T) {
	r := &Rules{Allow: []string{"Bash(git * push *)"}}
	ok := func(cmd string) bool {
		return Check(ModeDefault, r, "Bash", j(`{"command":"`+cmd+`"}`), "/r") == Allow
	}
	if !ok("git -c push origin") {
		t.Error("git * push * should match 'git -c push origin'")
	}
	if ok("git -c push") {
		t.Error("with two stars the trailing ' *' is not optional")
	}
	// escaped star is literal, not a wildcard
	r2 := &Rules{Allow: []string{`Bash(echo a\* b)`}}
	if Check(ModeDefault, r2, "Bash", j(`{"command":"echo a* b"}`), "/r") != Allow {
		t.Error(`a\* should match literal 'a* b'`)
	}
	if Check(ModeDefault, r2, "Bash", j(`{"command":"echo axb"}`), "/r") == Allow {
		t.Error(`a\* must not act as a wildcard`)
	}
}
