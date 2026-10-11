package repl

import (
	"strings"
	"testing"

	"github.com/gbasin/claude-go/internal/voice"
)

func newVoiceEditor(t *testing.T, cfg voice.Config) *editor {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	e := newEditor()
	vs := &voiceUI{events: make(chan voiceEvent, 8)}
	vs.cfg.Store(cfg)
	e.voice = vs
	return e
}

func TestVoiceAnchorInsert(t *testing.T) {
	e := newVoiceEditor(t, voice.Config{Enabled: true, Mode: "hold"})
	e.buf = []rune("fix the bug")
	e.pos = len(e.buf)

	e.voiceApply(voiceEvent{kind: veState, state: voice.StateRecording})
	if !e.voice.anchored || string(e.voice.prefix) != "fix the bug" || len(e.voice.suffix) != 0 {
		t.Fatalf("anchor: prefix=%q suffix=%q", e.voice.prefix, e.voice.suffix)
	}

	// interim rides inside the buffer, marked dim
	e.voiceApply(voiceEvent{kind: veInterim, text: "please"})
	if got := string(e.buf); got != "fix the bug please" {
		t.Fatalf("interim buf = %q", got)
	}
	if e.pos != len(e.buf) {
		t.Fatalf("pos = %d", e.pos)
	}
	if !strings.Contains(e.renderedBuf(), "\x1b[2mplease") {
		t.Fatalf("interim not dimmed: %q", e.renderedBuf())
	}

	// interim replaces itself from the anchor, not accumulates
	e.voiceApply(voiceEvent{kind: veInterim, text: "please help"})
	if got := string(e.buf); got != "fix the bug please help" {
		t.Fatalf("interim2 buf = %q", got)
	}

	// final splices at the anchor (replaces the interim text)
	e.voiceApply(voiceEvent{kind: veFinal, text: "please help me"})
	if got := string(e.buf); got != "fix the bug please help me" {
		t.Fatalf("final buf = %q", got)
	}
	if e.voice.interimLen != 0 {
		t.Fatal("interim range not cleared after final")
	}
	if string(e.voice.prefix) != "fix the bug please help me" {
		t.Fatalf("anchor not extended: %q", e.voice.prefix)
	}

	e.voiceApply(voiceEvent{kind: veState, state: voice.StateIdle})
	if e.voice.anchored {
		t.Fatal("anchor not cleared on idle")
	}
}

func TestVoiceAnchorMidBuffer(t *testing.T) {
	e := newVoiceEditor(t, voice.Config{Enabled: true, Mode: "hold"})
	e.buf = []rune("hello world")
	e.pos = 5 // cursor between "hello" and " world"

	e.voiceApply(voiceEvent{kind: veState, state: voice.StateRecording})
	e.voiceApply(voiceEvent{kind: veFinal, text: "beautiful"})
	want := "hello beautiful world"
	if got := string(e.buf); got != want {
		t.Fatalf("buf = %q want %q", got, want)
	}
	if e.pos != len("hello beautiful") {
		t.Fatalf("pos = %d", e.pos)
	}
}

func TestVoiceAnchorSpacing(t *testing.T) {
	e := newVoiceEditor(t, voice.Config{Enabled: true, Mode: "hold"})
	e.buf = []rune("trailing space ")
	e.pos = len(e.buf)
	e.voiceApply(voiceEvent{kind: veState, state: voice.StateRecording})
	e.voiceApply(voiceEvent{kind: veFinal, text: "next"})
	// prefix already ends in whitespace → no extra space
	if got := string(e.buf); got != "trailing space next" {
		t.Fatalf("buf = %q", got)
	}
}

func TestVoiceAutoSubmitTap(t *testing.T) {
	e := newVoiceEditor(t, voice.Config{Enabled: true, Mode: "tap"})
	e.voiceApply(voiceEvent{kind: veState, state: voice.StateRecording})
	r := e.voiceApply(voiceEvent{kind: veFinal, text: "one two three"})
	if r == nil || !r.ok || r.line != "one two three" {
		t.Fatalf("expected submit, got %+v", r)
	}
	if len(e.buf) != 0 {
		t.Fatalf("buffer not cleared after submit: %q", e.buf)
	}
}

func TestVoiceNoAutoSubmitShort(t *testing.T) {
	e := newVoiceEditor(t, voice.Config{Enabled: true, Mode: "tap"})
	e.voiceApply(voiceEvent{kind: veState, state: voice.StateRecording})
	r := e.voiceApply(voiceEvent{kind: veFinal, text: "two words"})
	if r != nil {
		t.Fatalf("2-word transcript submitted: %+v", r)
	}
	if got := string(e.buf); got != "two words" {
		t.Fatalf("buf = %q", got)
	}
}

func TestVoiceAutoSubmitHoldSetting(t *testing.T) {
	// hold mode only auto-submits via the autoSubmit setting
	e := newVoiceEditor(t, voice.Config{Enabled: true, Mode: "hold", AutoSubmit: true})
	e.voiceApply(voiceEvent{kind: veState, state: voice.StateRecording})
	r := e.voiceApply(voiceEvent{kind: veFinal, text: "a b c"})
	if r == nil || !r.ok {
		t.Fatal("autoSubmit setting not honored")
	}
	e2 := newVoiceEditor(t, voice.Config{Enabled: true, Mode: "hold"})
	e2.voiceApply(voiceEvent{kind: veState, state: voice.StateRecording})
	if r := e2.voiceApply(voiceEvent{kind: veFinal, text: "a b c"}); r != nil {
		t.Fatal("hold mode submitted without autoSubmit")
	}
}

func TestVoiceDivergedInput(t *testing.T) {
	e := newVoiceEditor(t, voice.Config{Enabled: true, Mode: "hold"})
	e.voiceApply(voiceEvent{kind: veState, state: voice.StateRecording})
	e.insert('x') // user types mid-session → input diverges
	before := string(e.buf)
	e.voiceApply(voiceEvent{kind: veFinal, text: "should not land"})
	if string(e.buf) != before {
		t.Fatalf("diverged input was clobbered: %q", e.buf)
	}
}

func TestVoiceCancelRestoresDraft(t *testing.T) {
	e := newVoiceEditor(t, voice.Config{Enabled: true, Mode: "hold"})
	e.buf = []rune("draft text")
	e.pos = 5
	e.voiceApply(voiceEvent{kind: veState, state: voice.StateRecording})
	e.voiceApply(voiceEvent{kind: veInterim, text: "interim junk"})
	e.voiceCancel()
	if got := string(e.buf); got != "draft text" {
		t.Fatalf("cancel buf = %q", got)
	}
	if e.pos != 5 {
		t.Fatalf("cancel pos = %d", e.pos)
	}
}

func TestVoiceWordCount(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"  ", 0},
		{"one", 1},
		{"one two three", 3},
		{"a-b-c", 3},      // word-like segments beat whitespace split (jZo max)
		{"don't stop", 2}, // apostrophe doesn't split the word
	}
	for _, c := range cases {
		if got := voiceWordCount(c.in); got != c.want {
			t.Errorf("voiceWordCount(%q) = %d want %d", c.in, got, c.want)
		}
	}
}

func TestVoiceSpaceBindingDefault(t *testing.T) {
	e := newVoiceEditor(t, voice.Config{})
	if e.kb["Chat\x00space"] != "voice:pushToTalk" {
		t.Fatalf("default space binding = %q", e.kb["Chat\x00space"])
	}
}
