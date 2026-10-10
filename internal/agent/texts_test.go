package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemBlocksFallback(t *testing.T) {
	t.Setenv("CLAUDE_GO_EXTRACTED_DIR", t.TempDir()) // empty → fallback
	cwd, bill := "/tmp/x", BillingMeta{}
	out := SystemBlocks("cli", cwd, "claude-fable-5-1", "", nil, nil, bill, 0)
	if len(out) != 3 {
		t.Fatalf("fallback blocks: %d, want 3", len(out))
	}
	main := out[2].Text
	if !strings.Contains(main, "interactive coding agent") {
		t.Fatalf("fallback main shape: %.120s", main)
	}
	if strings.Contains(main, "{{") {
		t.Fatalf("unresolved tokens in fallback main: %.200s", main)
	}
}

func TestSystemBlocksExtracted(t *testing.T) {
	d := t.TempDir()
	t.Setenv("CLAUDE_GO_EXTRACTED_DIR", d)
	cwd := t.TempDir()
	cwd, _ = filepath.EvalSymlinks(cwd)
	body := "HARVESTED MAIN TEXT\nworkdir is {{CWD}}\nmem {{MEMDIR}}\n"
	for _, class := range []string{"fable", "opus", "haiku"} {
		os.WriteFile(filepath.Join(d, "system_main_"+class+".txt"),
			[]byte(body), 0o644)
	}
	// meta.json marks the extraction present for probes
	os.WriteFile(filepath.Join(d, "meta.json"), []byte(`{"claude_version":"x"}`), 0o644)

	out := SystemBlocks("cli", cwd, "claude-fable-5-1", "", nil, nil, BillingMeta{}, 0)
	main := out[2].Text
	if !strings.HasPrefix(main, "HARVESTED MAIN TEXT") {
		t.Fatalf("extracted text not used: %.120s", main)
	}
	if strings.Contains(main, "{{") {
		t.Fatalf("unresolved tokens: %s", main)
	}
	if !strings.Contains(main, cwd) {
		t.Fatalf("{{CWD}} not rendered: %s", main)
	}
	// sessionExtras still append on the extracted path
	out = SystemBlocks("cli", cwd, "claude-fable-5-1", "", nil,
		[]string{"EXTRA-INSTRUCTION"}, BillingMeta{}, 0)
	if !strings.HasSuffix(out[2].Text, "EXTRA-INSTRUCTION") {
		t.Fatalf("sessionExtras dropped on extracted path: %.80s", out[2].Text)
	}
	// class routing: haiku model → haiku file
	out = SystemBlocks("cli", cwd, "claude-haiku-4-5", "", nil, nil, BillingMeta{}, 0)
	if !strings.Contains(out[2].Text, "HARVESTED") {
		t.Fatal("haiku class should also extract")
	}
}

func TestExtractedSystemMainMissing(t *testing.T) {
	t.Setenv("CLAUDE_GO_EXTRACTED_DIR", t.TempDir())
	if _, ok := extractedSystemMain("claude-fable-5-1", "/x", "cli"); ok {
		t.Fatal("missing extraction should miss")
	}
}

func TestModelClass(t *testing.T) {
	for m, want := range map[string]string{
		"claude-fable-5-1": "fable", "claude-opus-5-5": "opus",
		"claude-sonnet-5-5": "opus", "claude-haiku-4-5": "haiku",
		"weird": "haiku",
	} {
		if got := modelClass(m); got != want {
			t.Fatalf("%s → %s, want %s", m, got, want)
		}
	}
}
