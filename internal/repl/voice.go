package repl

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gbasin/claude-go/internal/agent"
	"github.com/gbasin/claude-go/internal/api"
	"github.com/gbasin/claude-go/internal/auth"
	"github.com/gbasin/claude-go/internal/config"
	"github.com/gbasin/claude-go/internal/voice"
)

// voiceUI is the editor-side voice state — the glue behind voice:pushToTalk
// and the /voice command. Mirrors the Tze() composer hook in mod_0747:
// anchor snapshot at recording start, interim transcript inserted as a dim
// range inside the buffer, final transcript spliced at the anchor,
// tap/autoSubmit auto-submit at >=3 words.
type voiceUI struct {
	ctl    *voice.Controller
	events chan voiceEvent
	cfg    atomic.Value // voice.Config — read from several goroutines
	mu     sync.Mutex

	// anchor: S.current/M.current/L.current in real
	anchored  bool
	prefix    []rune
	suffix    []rune
	committed string

	// dim interim range inside e.buf (interimRange in real)
	interimStart int
	interimLen   int
}

type voiceEvent struct {
	kind  int
	state voice.State
	text  string
}

const (
	veState = iota
	veInterim
	veFinal
	veError
)

func (e *editor) voiceChan() chan voiceEvent {
	if e.voice == nil {
		return nil // nil channel never fires — safe in select
	}
	return e.voice.events
}

// voiceEnabled is the nN() gate reduced to settings + auth presence.
func (e *editor) voiceEnabled() bool {
	cfg := voice.LoadConfig(e.voiceCWD)
	return cfg.Active()
}

// ensureVoice lazily builds the controller and event pump.
func (e *editor) ensureVoice() *voiceUI {
	if e.voice != nil {
		return e.voice
	}
	vs := &voiceUI{events: make(chan voiceEvent, 32)}
	vs.cfg.Store(voice.LoadConfig(e.voiceCWD))
	send := func(ev voiceEvent) {
		select {
		case vs.events <- ev:
		default:
			// editor isn't reading (mid-turn) — drop rather than wedge the
			// controller goroutine; interim loss is cosmetic
		}
	}
	vs.ctl = &voice.Controller{
		CheckAvailability: voice.CheckRecordingAvailability,
		StartCapture:      voice.StartRecorder,
		StopCapture:       func(r *voice.Recorder) { r.Stop() },
		ConnectStream: func(cb *voice.Callbacks, opts *voice.Options) *voice.Stream {
			opts.UserAgent = "claude-cli/" + api.Version + " (external, cli)"
			creds := e.voiceCreds
			if creds == nil {
				creds, _ = auth.Load()
			}
			tok := ""
			if creds != nil && !creds.IsAPIKey {
				tok, _ = creds.Token() // voice_stream needs OAuth (mn() in real)
			}
			return voice.Connect(cb, opts, tok, e.voiceBase)
		},
		// cached ws connect+close probe (~5min TTL): unreachable endpoint
		// surfaces "voice unavailable" before recording starts
		ProbeStream: func() bool { return voice.StreamReachable(e.voiceBase) },
		Keyterms:    func() []string { return voice.Keyterms(e.voiceCWD, nil) },
		Language: func() string {
			code, _ := voice.LanguageCode(vs.cfg.Load().(voice.Config).Language)
			return code
		},
		Mode: func() string { return vs.cfg.Load().(voice.Config).Mode },
		OnState: func(s voice.State) {
			send(voiceEvent{kind: veState, state: s})
		},
		OnInterim: func(t string) {
			send(voiceEvent{kind: veInterim, text: t})
		},
		OnFinal: func(t string) {
			send(voiceEvent{kind: veFinal, text: t})
		},
		OnError: func(msg string) {
			send(voiceEvent{kind: veError, text: msg})
		},
	}
	e.voice = vs
	return vs
}

// voiceToggle is the push-to-talk action: reload settings, gate on
// enabled, toggle the session.
func (e *editor) voiceToggle() {
	vs := e.ensureVoice()
	cfg := voice.LoadConfig(e.voiceCWD)
	vs.cfg.Store(cfg)
	if !cfg.Active() {
		compInline("\r\n  " + dim + "voice mode is off — /voice to enable" + reset + "\r\n")
		e.redraw()
		return
	}
	vs.ctl.Toggle()
}

// voiceCancel is esc/ctrl+c during a session: cancel + reset the anchor
// (restores the draft to its pre-recording contents — resetAnchor W in
// mod_0747).
func (e *editor) voiceCancel() {
	vs := e.voice
	if vs == nil {
		return
	}
	if vs.ctl != nil {
		vs.ctl.Cancel()
	}
	vs.mu.Lock()
	if vs.anchored {
		// restore value = prefix + suffix, cursor after prefix
		e.buf = append(append([]rune{}, vs.prefix...), vs.suffix...)
		e.pos = len(vs.prefix)
		vs.anchored = false
		vs.interimLen = 0
	}
	vs.mu.Unlock()
	e.redraw()
}

// voiceApply consumes a controller event on the editor's key goroutine.
// Returns a submit result when autoSubmit fires.
func (e *editor) voiceApply(ev voiceEvent) *readResult {
	vs := e.voice
	if vs == nil {
		return nil
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	switch ev.kind {
	case veState:
		switch ev.state {
		case voice.StateRecording:
			// snapshot anchor: value before/after cursor + committed text
			if !vs.anchored {
				vs.prefix = append([]rune{}, e.buf[:e.pos]...)
				vs.suffix = append([]rune{}, e.buf[e.pos:]...)
				vs.committed = string(e.buf)
				vs.anchored = true
				vs.interimLen = 0
			}
		case voice.StateIdle:
			vs.anchored = false
			vs.interimLen = 0
		}
		e.redraw()
	case veInterim:
		// interim insert effect [X,l] in Tze(): rebuild buffer around the
		// anchor with the interim text in place
		if !vs.anchored || string(e.buf) != vs.committed {
			return nil // real: bail when input diverged
		}
		x := []rune(ev.text)
		pre, suf := vs.prefix, vs.suffix
		var nb []rune
		sp1 := len(pre) > 0 && !isSpaceRune(pre[len(pre)-1]) && len(x) > 0
		sp2 := len(suf) > 0 && !isSpaceRune(suf[0])
		nb = append(nb, pre...)
		if sp1 {
			nb = append(nb, ' ')
		}
		vs.interimStart = len(nb)
		nb = append(nb, x...)
		vs.interimLen = len(x)
		if sp2 {
			nb = append(nb, ' ')
		}
		nb = append(nb, suf...)
		e.buf = nb
		e.pos = vs.interimStart + len(x)
		vs.committed = string(nb)
		e.redraw()
	case veFinal:
		q := ev.text
		if !vs.anchored {
			return nil // "voice_transcript_insert","no_anchor"
		}
		if string(e.buf) != vs.committed {
			return nil // "input_diverged" — user typed mid-session
		}
		pre, suf := vs.prefix, vs.suffix
		qb := []rune(q)
		sp1 := len(pre) > 0 && !isSpaceRune(pre[len(pre)-1]) && len(qb) > 0
		sp2 := len(suf) > 0 && !isSpaceRune(suf[0]) && len(qb) > 0
		var nb []rune
		nb = append(nb, pre...)
		if sp1 {
			nb = append(nb, ' ')
		}
		nb = append(nb, qb...)
		cur := len(nb)
		if sp2 {
			nb = append(nb, ' ')
		}
		nb = append(nb, suf...)
		e.buf = nb
		e.pos = cur
		vs.committed = string(nb)
		// extend the anchor past the committed final so the next interim
		// appends after it (S.current = te+Ve+q)
		vs.prefix = append([]rune{}, nb[:cur]...)
		vs.interimLen = 0
		e.redraw()
		// autoSubmit: tap mode OR voice.autoSubmit, transcript >= 3 words
		// (jZo word count) → l.submit(J, true)
		vc := vs.cfg.Load().(voice.Config)
		if (vc.Mode == "tap" || vc.AutoSubmit) && voiceWordCount(q) >= 3 {
			e.closeBox()
			return &readResult{line: e.submitLine(), ok: true}
		}
	case veError:
		compInline(fmt.Sprintf("\r\n%s  %s%s\r\n", red, ev.text, reset))
		e.redraw()
	}
	return nil
}

// voiceHint is the composer voice indicator — mCe() in mod_0747: red
// "● REC · tap to send" in tap mode, "listening…" in hold, spinner while
// processing.
func (e *editor) voiceHint() string {
	if e.voice == nil || e.voice.ctl == nil {
		return ""
	}
	switch e.voice.ctl.CurrentState() {
	case voice.StateRecording:
		vc, _ := e.voice.cfg.Load().(voice.Config)
		if vc.Mode == "tap" {
			return red + "● REC" + reset + dim + " · tap space to send" + reset
		}
		return dim + "listening…" + reset
	case voice.StateProcessing:
		return dim + "transcribing…" + reset
	}
	return ""
}

// renderedBuf returns e.buf with the interim voice range wrapped in dim
// escapes — the interimRange styling from real's editor.
// promptSuggestions are real's dim ghost hints in the empty input —
// "Try "refactor <filepath>"" style, gated by promptSuggestionEnabled.
var ghostIdx atomic.Int64
var promptSuggestions = []string{
	`Try "refactor <filepath>"`,
	`Try "create a util logging.py that..."`,
	`Try "edit <filepath> to..."`,
	`Try "fix the type errors"`,
	`Try "run the tests and fix failures"`,
	`Try "summarize this codebase"`,
}

func promptSuggestionsEnabled() bool {
	// --proactivity off also gates the ghosts (proactivityLevel === 0)
	if v := os.Getenv("CLAUDE_GO_PROACTIVITY"); v == "off" || v == "0" {
		return false
	}
	if v, ok := config.UserSetting("promptSuggestionEnabled").(bool); ok {
		return v
	}
	// real's rotating suggestions are flag-gated — the eval only resolves
	// on the first-party endpoint; custom base URLs / API-key setups show
	// a bare prompt (verified: sandboxed real renders '❯ ' alone)
	if !api.IsFirstPartyBaseURL() {
		return false
	}
	return true
}

// histGhost returns the autosuggest tail for the current buffer — the
// newest history line that starts with it (fish-style; real's
// inlineGhostText source). Empty when nothing matches.
func (e *editor) histGhost() string {
	if len(e.buf) == 0 || e.pos < len(e.buf) || e.histSearch || e.acOpen {
		return ""
	}
	pfx := string(e.buf)
	for i := len(e.history) - 1; i >= 0; i-- {
		h := e.history[i]
		// single-line ghosts only — a multiline tail would break the
		// redraw's row accounting
		if len(h) > len(pfx) && strings.HasPrefix(h, pfx) && !strings.Contains(h, "\n") {
			return h[len(pfx):]
		}
	}
	return ""
}

func ghostText() string {
	if !promptSuggestionsEnabled() {
		return ""
	}
	return dim + promptSuggestions[int(ghostIdx.Load())%len(promptSuggestions)] + reset
}

func (e *editor) renderedBuf() string {
	// shift+arrow selection renders reverse-video over the span (real's
	// selectionAnchor rendering)
	if a, b, ok := e.selSpan(); ok {
		return string(e.buf[:a]) + "[7m" + string(e.buf[a:b]) + reset + string(e.buf[b:])
	}
	vs := e.voice
	if vs == nil || vs.interimLen == 0 {
		if len(e.buf) == 0 && !e.histSearch {
			return ghostText()
		}
		// mode badges: a leading # or ! renders with real's badge colors —
		// # = "remember" on memoryBackgroundColor, ! = bashBorder
		// (badges.go modeBadgeRender; audits/tui.md §66)
		if len(e.buf) > 0 && (e.buf[0] == '#' || e.buf[0] == '!') {
			return modeBadgeRender(string(e.buf))
		}
		// inlineGhostText — history autosuggestion: a history entry whose
		// text starts with the buffer renders its tail dimmed after the
		// cursor (real's inline ghost); → at EOL accepts it
		if g := e.histGhost(); g != "" {
			return string(e.buf) + dim + g + reset
		}
		return string(e.buf)
	}
	s := vs.interimStart
	en := s + vs.interimLen
	if s < 0 || en > len(e.buf) {
		return string(e.buf)
	}
	return string(e.buf[:s]) + dim + string(e.buf[s:en]) + reset + string(e.buf[en:])
}

// voiceCmd implements /voice [hold|tap|off] — the D() command in
// mod_1000 with real's message texts. The policy gate (allow_voice_mode),
// feature flag, and NAPI mic-permission steps don't exist here; sox's own
// capture failure surfaces the permission error instead.
func voiceCmd(l *agent.Loop, ed *editor, arg string) {
	a := strings.ToLower(strings.TrimSpace(arg))
	var mode string
	switch a {
	case "", "hold", "tap", "off":
		mode = a
	default:
		fmt.Printf("  Unknown mode: %q. Use hold, tap, or off.\n", strings.TrimSpace(arg))
		return
	}
	cwd := ""
	if l != nil && l.ToolCtx != nil {
		cwd = l.ToolCtx.CWD
	}
	if cwd == "" {
		cwd = ed.voiceCWD
	}
	cfg := voice.LoadConfig(cwd)
	// off / bare-toggle-while-enabled → disable (real checks voiceEnabled)
	if mode == "off" || (arg == "" && cfg.Active()) {
		if err := voice.Disable(); err != nil {
			fmt.Println("  Failed to update settings. Check your settings file for syntax errors.")
			return
		}
		fmt.Println("  Voice mode disabled.")
		return
	}
	// enable path — availability probes run before the settings write
	creds, err := auth.Load()
	if err != nil || creds == nil || creds.IsAPIKey {
		fmt.Println("  Voice mode requires a Claude.ai account. Please run /login to sign in.")
		return
	}
	if av := voice.CheckRecordingAvailability(); !av.Available {
		reason := av.Reason
		if reason == "" {
			reason = "Voice mode is not available in this environment."
		}
		fmt.Println(" ", strings.ReplaceAll(reason, "\n", "\n  "))
		return
	}
	if deps := voice.CheckVoiceDependencies(); !deps.Available {
		msg := "No audio recording tool found."
		if deps.InstallCommand != "" {
			msg += " Install audio recording tools? Run: " + deps.InstallCommand
		} else {
			msg += " Install SoX manually for audio recording."
		}
		fmt.Println(" ", msg)
		return
	}
	// requestMicrophonePermission (real's le): on darwin a quick rec
	// spawn may trigger the TCC prompt; a probe failure maps to the
	// no-mic text
	if !voice.RequestMicrophonePermission() {
		fmt.Println(" ", strings.ReplaceAll(voice.NoMicText(), "\n", "\n  "))
		return
	}
	m := mode
	if m == "" {
		m = cfg.Mode
	}
	if err := voice.Enable(m); err != nil {
		fmt.Println("  Failed to update settings. Check your settings file for syntax errors.")
		return
	}
	hint := "Hold space to record."
	if m == "tap" {
		hint = "Tap space (with input empty) to start, tap again to send."
	}
	fmt.Printf("  Voice mode enabled (%s). %s%s\n", m, hint, langHintNote(cwd))
}

// langHintNote reproduces the dictation-language note real appends to the
// enable message (fellBackFrom warning or the first-two-uses hint, tracked
// via voiceLangHint* counters in ~/.claude.json).
func langHintNote(cwd string) string {
	cfg := voice.LoadConfig(cwd)
	code, fell := voice.LanguageCode(cfg.Language)
	if fell != "" {
		return fmt.Sprintf(" Note: %q is not a supported dictation language; using English. Change it via /config.", fell)
	}
	g, path := config.ReadClaudeJSON()
	last, _ := g["voiceLangHintLastLanguage"].(string)
	count := 0
	if last == code {
		if f, ok := g["voiceLangHintShownCount"].(float64); ok {
			count = int(f)
		}
	} else {
		count = 0
	}
	if count >= 2 {
		return ""
	}
	g["voiceLangHintShownCount"] = count + 1
	g["voiceLangHintLastLanguage"] = code
	config.WriteClaudeJSON(path, g)
	return fmt.Sprintf(" Dictation language: %s (/config to change).", code)
}

func isSpaceRune(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

// voiceWordCount is jZo(): max(whitespace-split count, word-like segment
// count). The segment count approximates Intl.Segmenter word granularity
// with runs of letters/digits.
func voiceWordCount(s string) int {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0
	}
	ws := len(strings.Fields(t))
	wl := 0
	inWord := false
	for _, r := range t {
		word := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r > 127 || r == '\'' || r == '_'
		if word && !inWord {
			wl++
		}
		inWord = word
	}
	if wl > ws {
		return wl
	}
	return ws
}
