package main

import (
	"testing"
)

// ANTHROPIC_BETAS env betas: comma-separated, trimmed, empties dropped,
// afk-mode entries filtered (mod_0137.js).
func TestEnvBetas(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"", ""},
		{"a-1,b-2", "a-1,b-2"},
		{" a-1 , , b-2 ", "a-1,b-2"},
		{"afk-mode-2026-01-31", ""},
		{"x,afk-mode-2026-01-31,y", "x,y"},
	} {
		t.Setenv("ANTHROPIC_BETAS", tt.in)
		if got := envBetas(); got != tt.want {
			t.Errorf("%q: got %q want %q", tt.in, got, tt.want)
		}
	}
}

// ANTHROPIC_DEFAULT_MODEL (dLe/mv in mod_0137.js): sentinels mean "no
// override"; the value flows through the same alias resolution as --model.
func TestDefaultModelEnv(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"", ""},
		{"default", ""},
		{"INHERIT", ""},
		{"  inherit  ", ""},
		{"claude-x", "claude-x"},
		{" opus ", "claude-opus-5-5"},
	} {
		t.Setenv("ANTHROPIC_DEFAULT_MODEL", tt.in)
		if got := resolveModel(defaultModelEnv()); got != tt.want {
			t.Errorf("%q: got %q want %q", tt.in, got, tt.want)
		}
	}
}

// --model default resolves through ANTHROPIC_DEFAULT_MODEL.
func TestResolveModelDefaultUsesEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_DEFAULT_MODEL", "custom-default-1")
	if got := resolveModel("default"); got != "custom-default-1" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("ANTHROPIC_DEFAULT_MODEL", "")
	if got := resolveModel("default"); got != "" {
		t.Fatalf("got %q", got)
	}
}

// --max-turns marks the flag as set so CLAUDE_CODE_MAX_TURNS can't shadow it.
func TestMaxTurnsSet(t *testing.T) {
	o := parseCLI([]string{"cg", "--max-turns", "7", "-p", "hi"})
	if !o.maxTurnsSet || o.maxTurns != 7 {
		t.Fatalf("maxTurns=%d set=%v", o.maxTurns, o.maxTurnsSet)
	}
	o = parseCLI([]string{"cg", "-p", "hi"})
	if o.maxTurnsSet {
		t.Fatal("maxTurnsSet should be false without --max-turns")
	}
}

// envOn mirrors real's Le() — strict boolean, not "any non-empty".
func TestEnvOn(t *testing.T) {
	for _, tt := range []struct {
		v    string
		want bool
	}{
		{"1", true}, {"true", true}, {"yes", true}, {"on", true},
		{" TRUE ", true}, {"", false}, {"0", false}, {"false", false},
		{"no", false}, {"off", false}, {"2", false}, {"enabled", false},
	} {
		t.Setenv("CLAUDE_CODE_X_T", tt.v)
		if got := envOn("CLAUDE_CODE_X_T"); got != tt.want {
			t.Errorf("%q: got %v want %v", tt.v, got, tt.want)
		}
	}
}
