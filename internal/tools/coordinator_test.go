package tools

import (
	"os"
	"testing"
)

// stubCoordinatorSurface replaces the launch-surface predicates with
// settable flags so tests don't depend on real detection.
func stubCoordinatorSurface(t *testing.T) (interactive, remote *bool) {
	t.Helper()
	oldI, oldR := CoordinatorIsInteractive, CoordinatorWorkspaceRemote
	i, r := false, false
	CoordinatorIsInteractive = func() bool { return i }
	CoordinatorWorkspaceRemote = func() bool { return r }
	t.Cleanup(func() {
		CoordinatorIsInteractive, CoordinatorWorkspaceRemote = oldI, oldR
		os.Unsetenv("CLAUDE_CODE_COORDINATOR_MODE")
		os.Unsetenv("CLAUDE_CODE_REMOTE")
	})
	return &i, &r
}

func TestIsCoordinatorModeEnv(t *testing.T) {
	stubCoordinatorSurface(t)
	if IsCoordinatorMode() {
		t.Fatal("unset env should not be coordinator")
	}
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " 1 "} {
		os.Setenv("CLAUDE_CODE_COORDINATOR_MODE", v)
		if !IsCoordinatorMode() {
			t.Fatalf("%q should enable coordinator mode", v)
		}
	}
	for _, v := range []string{"0", "false", "no", "off", "", "2", "enabled"} {
		os.Setenv("CLAUDE_CODE_COORDINATOR_MODE", v)
		if IsCoordinatorMode() {
			t.Fatalf("%q should not enable coordinator mode", v)
		}
	}
}

func TestCoordinatorInteractiveGate(t *testing.T) {
	interactive, remote := stubCoordinatorSurface(t)
	// Ta(): interactive + local workspace + no CLAUDE_CODE_REMOTE => off
	// even with the env var set.
	*interactive = true
	os.Setenv("CLAUDE_CODE_COORDINATOR_MODE", "1")
	if IsCoordinatorMode() {
		t.Fatal("interactive local session should not be coordinator")
	}
	os.Setenv("CLAUDE_CODE_REMOTE", "1")
	if !IsCoordinatorMode() {
		t.Fatal("CLAUDE_CODE_REMOTE lifts the interactive gate")
	}
	os.Unsetenv("CLAUDE_CODE_REMOTE")
	*remote = true
	if !IsCoordinatorMode() {
		t.Fatal("remote workspace lifts the interactive gate")
	}
	*interactive, *remote = false, false
	if !IsCoordinatorMode() {
		t.Fatal("non-interactive session honors the env flag")
	}
}

func TestSessionKind(t *testing.T) {
	stubCoordinatorSurface(t)
	if SessionKind() != "normal" {
		t.Fatal("expected normal")
	}
	os.Setenv("CLAUDE_CODE_COORDINATOR_MODE", "1")
	if SessionKind() != "coordinator" {
		t.Fatal("expected coordinator")
	}
}

func TestMatchSessionMode(t *testing.T) {
	stubCoordinatorSurface(t)
	if msg := MatchSessionMode(""); msg != "" {
		t.Fatalf("empty mode: %q", msg)
	}
	if msg := MatchSessionMode("normal"); msg != "" {
		t.Fatalf("already normal: %q", msg)
	}
	if msg := MatchSessionMode("coordinator"); msg != "Entered coordinator mode to match resumed session." {
		t.Fatalf("enter: %q", msg)
	}
	if !IsCoordinatorMode() {
		t.Fatal("env var should be set after enter")
	}
	if msg := MatchSessionMode("coordinator"); msg != "" {
		t.Fatalf("already coordinator: %q", msg)
	}
	if msg := MatchSessionMode("normal"); msg != "Exited coordinator mode to match resumed session." {
		t.Fatalf("exit: %q", msg)
	}
	if IsCoordinatorMode() {
		t.Fatal("env var should be cleared after exit")
	}
}
