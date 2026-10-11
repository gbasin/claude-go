package repl

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gbasin/claude-go/internal/api"
)

// persistLocalSetting merges, writes, and deletes keys in
// .claude/settings.local.json without clobbering existing content.
func TestPersistLocalSetting(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd) // keep ensureGitignoreLocal away from the real repo
	os.MkdirAll(filepath.Join(cwd, ".claude"), 0o755)
	os.WriteFile(filepath.Join(cwd, ".claude", "settings.local.json"),
		[]byte(`{"theme":"dark"}`), 0o600)

	persistLocalSetting(cwd, "advisorModel", "claude-opus-4-6")
	persistLocalSetting(cwd, "autoCompactWindow", "500k")
	persistLocalSetting(cwd, "advisorModel", nil) // off deletes

	var st map[string]any
	b, _ := os.ReadFile(filepath.Join(cwd, ".claude", "settings.local.json"))
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st["theme"] != "dark" {
		t.Fatalf("existing key lost: %v", st)
	}
	if st["autoCompactWindow"] != "500k" {
		t.Fatalf("autoCompactWindow not written: %v", st)
	}
	if _, ok := st["advisorModel"]; ok {
		t.Fatalf("advisorModel should be deleted on off: %v", st)
	}
}

// writeUserSettingsEnv merges provider keys into the env block of
// ~/.claude/settings.json (CLAUDE_CONFIG_DIR override).
func TestWriteUserSettingsEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"env":{"KEEP":"1"},"model":"sonnet"}`), 0o600)

	writeUserSettingsEnv(map[string]string{
		"CLAUDE_CODE_USE_BEDROCK": "1",
		"AWS_REGION":              "us-west-2",
	})

	var st map[string]any
	b, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	env, _ := st["env"].(map[string]any)
	if env["KEEP"] != "1" || env["CLAUDE_CODE_USE_BEDROCK"] != "1" || env["AWS_REGION"] != "us-west-2" {
		t.Fatalf("env merge wrong: %v", env)
	}
	if st["model"] != "sonnet" {
		t.Fatalf("non-env key lost: %v", st)
	}
}

// settings-review accept writes proposedContent when the on-disk file still
// matches baseSha256; a drifted file is refused and stays staged (mod_1337).
func TestSettingsReviewAccept(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	target := filepath.Join(home, "proj", "settings.json")
	os.MkdirAll(filepath.Dir(target), 0o755)
	base := []byte(`{"a":1}`)
	os.WriteFile(target, base, 0o600)
	proposed := `{"a":2}`
	sum := fmt.Sprintf("%x", sha256.Sum256(base))
	entry := settingsReviewEntry{
		ID:              "e1",
		Path:            target,
		ResolvedPath:    target,
		BaseSha256:      &sum,
		ProposedSha256:  fmt.Sprintf("%x", sha256.Sum256([]byte(proposed))),
		ProposedContent: proposed,
	}
	entry.By.Tool = "Write"
	saveSettingsReview([]settingsReviewEntry{entry})

	entries, err := loadSettingsReview()
	if err != nil || len(entries) != 1 {
		t.Fatalf("load: %v %v", entries, err)
	}
	if err := applyStagedChange(entries[0]); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != proposed {
		t.Fatalf("target not updated: %s", got)
	}
}

func TestSettingsReviewConflict(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	target := filepath.Join(home, "settings.json")
	os.WriteFile(target, []byte(`{"a":9}`), 0o600) // drifted since staging
	stale := fmt.Sprintf("%x", sha256.Sum256([]byte(`{"a":1}`)))
	proposed := `{"a":2}`
	e := settingsReviewEntry{
		ID:              "e1",
		Path:            target,
		ResolvedPath:    target,
		BaseSha256:      &stale,
		ProposedSha256:  fmt.Sprintf("%x", sha256.Sum256([]byte(proposed))),
		ProposedContent: proposed,
	}
	if err := applyStagedChange(e); err == nil {
		t.Fatal("expected conflict refusal")
	}
	got, _ := os.ReadFile(target)
	if string(got) != `{"a":9}` {
		t.Fatal("conflict must not write")
	}
}

func TestNthLastAssistantText(t *testing.T) {
	mk := func(role, text string) api.Message {
		return api.TextMessage(role, text)
	}
	msgs := []api.Message{
		mk("user", "q1"),
		mk("assistant", "first"),
		mk("user", "q2"),
		mk("assistant", "second"),
	}
	if got := nthLastAssistantText(msgs, 1); got != "second" {
		t.Fatalf("n=1: %q", got)
	}
	if got := nthLastAssistantText(msgs, 2); got != "first" {
		t.Fatalf("n=2: %q", got)
	}
	if got := nthLastAssistantText(msgs, 3); got != "" {
		t.Fatalf("n=3: %q", got)
	}
}

func TestMaskEnv(t *testing.T) {
	if got := maskEnv("ANTHROPIC_API_KEY", "sk-ant-secret-value"); got != "sk-a…alue" {
		t.Fatalf("key not masked: %q", got)
	}
	if got := maskEnv("AWS_REGION", "us-east-1"); got != "us-east-1" {
		t.Fatalf("non-secret masked: %q", got)
	}
}

func TestRestartArgs(t *testing.T) {
	orig := os.Args
	defer func() { os.Args = orig }()
	os.Args = []string{"claude-go", "--model", "opus", "-r", "old-session", "-c"}
	got := restartArgs("new-id")
	want := []string{"claude-go", "--model", "opus", "-r", "new-id"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
