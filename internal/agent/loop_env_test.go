package agent

import (
	"strings"
	"testing"
)

func TestSubagentModelEnv(t *testing.T) {
	t.Run("pin applies when subagent inherited parent", func(t *testing.T) {
		t.Setenv("CLAUDE_GO_MAIN_LOOP_MODEL", "claude-opus-5-5")
		t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "custom-sub-model")
		l := &Loop{IsSubagent: true, Model: "claude-opus-5-5"}
		if got := l.subagentModel(); got != "custom-sub-model" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("pin keeps agent-chosen model", func(t *testing.T) {
		t.Setenv("CLAUDE_GO_MAIN_LOOP_MODEL", "claude-opus-5-5")
		t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "custom-sub-model")
		l := &Loop{IsSubagent: true, Model: "claude-haiku-4-5-20251001"}
		if got := l.subagentModel(); got != "claude-haiku-4-5-20251001" {
			t.Fatalf("agent-chosen model should win over env pin, got %q", got)
		}
	})
	t.Run("inherit sentinel", func(t *testing.T) {
		t.Setenv("CLAUDE_GO_MAIN_LOOP_MODEL", "claude-opus-5-5")
		t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "inherit")
		l := &Loop{IsSubagent: true, Model: "claude-opus-5-5"}
		if got := l.subagentModel(); got != "claude-opus-5-5" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("FORCE strips agent choice -> parent", func(t *testing.T) {
		t.Setenv("CLAUDE_GO_MAIN_LOOP_MODEL", "claude-opus-5-5")
		t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL_FORCE", "1")
		l := &Loop{IsSubagent: true, Model: "claude-haiku-4-5-20251001"}
		if got := l.subagentModel(); got != "claude-opus-5-5" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("FORCE + pin -> pin", func(t *testing.T) {
		t.Setenv("CLAUDE_GO_MAIN_LOOP_MODEL", "claude-opus-5-5")
		t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "sonnet")
		t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL_FORCE", "1")
		l := &Loop{IsSubagent: true, Model: "claude-haiku-4-5-20251001"}
		if got := l.subagentModel(); got != "claude-sonnet-5-5" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("pin resolves aliases and env overrides", func(t *testing.T) {
		t.Setenv("CLAUDE_GO_MAIN_LOOP_MODEL", "claude-opus-5-5")
		t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "opus")
		t.Setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "my-opus-id")
		l := &Loop{IsSubagent: true, Model: "claude-opus-5-5"}
		if got := l.subagentModel(); got != "my-opus-id" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestMaxOutputTokensEnv(t *testing.T) {
	for _, tt := range []struct {
		v    string
		want int
		ok   bool
	}{
		{"", 0, false},
		{"  ", 0, false},
		{"8000", 8000, true},
		{"0", 0, false},
		{"-5", 0, false},
		{"abc", 0, false},
		{"64000.0", 64000, true},
	} {
		t.Setenv("CLAUDE_CODE_MAX_OUTPUT_TOKENS", tt.v)
		got, ok := maxOutputTokensEnv()
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("%q: got (%d,%v) want (%d,%v)", tt.v, got, ok, tt.want, tt.ok)
		}
	}
}

func TestThinkingConfigEnvs(t *testing.T) {
	t.Run("MAX_THINKING_TOKENS enables with budget", func(t *testing.T) {
		t.Setenv("MAX_THINKING_TOKENS", "5000")
		l := &Loop{}
		if got := string(l.thinkingConfig(64000)); got != `{"type":"enabled","budget_tokens":5000}` {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("MAX_THINKING_TOKENS=0 disables", func(t *testing.T) {
		t.Setenv("MAX_THINKING_TOKENS", "0")
		l := &Loop{ThinkingMode: "enabled"}
		if got := string(l.thinkingConfig(64000)); got != `{"type":"disabled"}` {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("MAX_THINKING_TOKENS invalid disables", func(t *testing.T) {
		t.Setenv("MAX_THINKING_TOKENS", "abc")
		l := &Loop{}
		if got := string(l.thinkingConfig(64000)); got != `{"type":"disabled"}` {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("MAX_THINKING_TOKENS clamps to mt-1, min 1024", func(t *testing.T) {
		t.Setenv("MAX_THINKING_TOKENS", "999999")
		l := &Loop{}
		if got := string(l.thinkingConfig(2000)); !strings.Contains(got, `"budget_tokens":1999`) {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("DISABLE_THINKING omits key", func(t *testing.T) {
		t.Setenv("CLAUDE_CODE_DISABLE_THINKING", "1")
		l := &Loop{ThinkingMode: "enabled", ThinkingBudget: 4000}
		if got := l.thinkingConfig(64000); got != nil {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("explicit disabled beats MAX_THINKING_TOKENS", func(t *testing.T) {
		t.Setenv("MAX_THINKING_TOKENS", "5000")
		l := &Loop{ThinkingMode: "disabled"}
		if got := string(l.thinkingConfig(64000)); got != `{"type":"disabled"}` {
			t.Fatalf("got %q", got)
		}
	})
}

func TestPromptCacheTTLEnvs(t *testing.T) {
	t.Run("default main=1h subagent=none", func(t *testing.T) {
		if got := (&Loop{}).promptCacheTTL(); got != "1h" {
			t.Fatalf("main %q", got)
		}
		if got := (&Loop{IsSubagent: true}).promptCacheTTL(); got != "" {
			t.Fatalf("subagent %q", got)
		}
	})
	t.Run("CLAUDE_CODE_PROMPT_CACHE_TTL=5m drops ttl", func(t *testing.T) {
		t.Setenv("CLAUDE_CODE_PROMPT_CACHE_TTL", "5m")
		if got := (&Loop{}).promptCacheTTL(); got != "" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("SUBAGENT override grants 1h", func(t *testing.T) {
		t.Setenv("CLAUDE_CODE_SUBAGENT_PROMPT_CACHE_TTL", "1h")
		if got := (&Loop{IsSubagent: true}).promptCacheTTL(); got != "1h" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("FORCE_PROMPT_CACHING_5M wins", func(t *testing.T) {
		t.Setenv("FORCE_PROMPT_CACHING_5M", "1")
		t.Setenv("CLAUDE_CODE_PROMPT_CACHE_TTL", "1h")
		if got := (&Loop{}).promptCacheTTL(); got != "" {
			t.Fatalf("got %q", got)
		}
	})
}
