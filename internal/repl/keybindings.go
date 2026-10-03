package repl

// keybindings.json remap layer (see mod_0245 default table + mod_0643 docs).
//
// File: ~/.claude/keybindings.json
//
//	{"bindings":[
//	  {"context":"Chat","bindings":{"ctrl+e":"chat:externalEditor","ctrl+u":null}}
//	]}
//
// Contexts supported here: Chat (prompt editor) and Global. A string value
// rebinds the key to an action; null unbinds the default. Chords are
// space-separated keystrokes ("ctrl+x ctrl+k") with a 3s timeout between
// keystrokes. Canonical keystroke form is modifiers in fixed order joined
// with '+' plus a key name: "ctrl+alt+shift+cmd+<name>".

import (
	"claude-go/internal/config"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var kbModAliases = map[string]int{
	"ctrl": modCtrl, "control": modCtrl,
	"alt": modAlt, "opt": modAlt, "option": modAlt,
	"meta":  modAlt, // alt and meta are identical in terminals
	"shift": modShift,
	"cmd":   modCmd, "command": modCmd, "super": modCmd, "win": modCmd,
}

var kbSpecialKeys = map[string]bool{
	"escape": true, "enter": true, "tab": true, "space": true,
	"backspace": true, "delete": true, "insert": true,
	"up": true, "down": true, "left": true, "right": true,
	"home": true, "end": true, "pageup": true, "pagedown": true,
}

// kbActions maps action strings to editor behavior.
var kbActions = map[string]func(*editor) *readResult{
	"app:interrupt":   func(e *editor) *readResult { return e.interruptKey() },
	"app:exit":        func(e *editor) *readResult { return e.exitKey() },
	"app:toggleTodos": func(e *editor) *readResult { e.toggleTodos(); return nil },
	"app:toggleTranscript": func(e *editor) *readResult {
		e.toggleExpand()
		return nil
	},
	"history:search":      func(e *editor) *readResult { e.startSearch(); return nil },
	"chat:cancel":         func(e *editor) *readResult { return e.cancelKey() },
	"chat:clearInput":     func(e *editor) *readResult { e.clearInput(); return nil },
	"chat:cycleMode":      func(e *editor) *readResult { e.cyclePermMode(); return nil },
	"chat:killAgents":     func(e *editor) *readResult { e.killAgents(); return nil },
	"chat:externalEditor": func(e *editor) *readResult { return e.externalEditor() },
	"chat:imagePaste":     func(e *editor) *readResult { return e.imagePaste() },
	"chat:submit":         func(e *editor) *readResult { return e.submitKey() },
	"chat:newline":        func(e *editor) *readResult { e.insert('\n'); return nil },
	"chat:undo":           func(e *editor) *readResult { e.undo(); return nil },
	"chat:sendNow":        func(e *editor) *readResult { return e.sendNow() },
	"chat:queueSubmit": func(e *editor) *readResult {
		return e.queueSubmitKey()
	},
	"chat:modelPicker":    func(e *editor) *readResult { return e.modelPickerKey() },
	"chat:fastMode":       func(e *editor) *readResult { e.toggleFastMode(); return nil },
	"chat:thinkingToggle": func(e *editor) *readResult { e.toggleThinking(); return nil },
	"chat:stash":          func(e *editor) *readResult { e.stashLine(); return nil },
	"app:toggleBrief":     func(e *editor) *readResult { toggleBrief(); return nil },
	// voice:pushToTalk — real binds space in Chat; the space-press path is
	// handled in insertKey (printable chars aren't keystrokes). This entry
	// lets users rebind to a modifier combo via keybindings.json.
	"voice:pushToTalk": func(e *editor) *readResult { e.voiceToggle(); return nil },
}

// defaultKeybindings mirrors the relevant rows of the CC binding table.
var defaultKeybindings = []struct {
	context string
	chord   string
	action  string
}{
	{"Global", "ctrl+c", "app:interrupt"},
	{"Global", "ctrl+d", "app:exit"},
	{"Global", "ctrl+t", "app:toggleTodos"},
	{"Global", "ctrl+o", "app:toggleTranscript"},
	{"Global", "ctrl+r", "history:search"},
	{"Chat", "escape", "chat:cancel"},
	{"Chat", "ctrl+l", "chat:clearInput"},
	{"Chat", "ctrl+x ctrl+k", "chat:killAgents"},
	{"Chat", "ctrl+x ctrl+e", "chat:externalEditor"},
	{"Chat", "shift+tab", "chat:cycleMode"},
	{"Chat", "ctrl+g", "chat:externalEditor"},
	{"Chat", "ctrl+v", "chat:imagePaste"},
	{"Chat", "alt+v", "chat:imagePaste"},
	// rest of the real Chat/Global table
	{"Global", "ctrl+shift+b", "app:toggleBrief"},
	{"Chat", "ctrl+j", "chat:newline"},
	{"Chat", "ctrl+_", "chat:undo"},
	{"Chat", "ctrl+-", "chat:undo"},
	{"Chat", "ctrl+s", "chat:stash"},
	{"Chat", "meta+p", "chat:modelPicker"},
	{"Chat", "meta+o", "chat:fastMode"},
	{"Chat", "meta+t", "chat:thinkingToggle"},
	{"Chat", "ctrl+x enter", "chat:queueSubmit"},
	{"Chat", "ctrl+x ctrl+s", "chat:sendNow"},
	{"Chat", "ctrl+enter", "chat:sendNow"},
	{"Chat", "space", "voice:pushToTalk"}, // real's default (mod_0245)
}

// keystroke renders a decoded key into canonical stroke form, or "" if the
// key isn't bindable (plain printable text, paste content, unknown seq).
func keystroke(k key) string {
	var name string
	mods := int(k.mod) & 0xF
	if int(k.mod)&32 != 0 { // kitty meta bit -> alt (identical in terminals)
		mods |= modAlt
	}
	switch {
	case k.paste != "":
		return ""
	case k.seq != "":
		switch k.seq {
		case "[A":
			name = "up"
		case "[B":
			name = "down"
		case "[C":
			name = "right"
		case "[D":
			name = "left"
		case "[H", "[1~", "[7~":
			name = "home"
		case "[F", "[4~", "[8~":
			name = "end"
		case "[Z":
			name = "tab"
			mods |= modShift
		case "[2~":
			name = "insert"
		case "[3~":
			name = "delete"
		case "[5~":
			name = "pageup"
		case "[6~":
			name = "pagedown"
		default:
			return ""
		}
	case k.b == 0x1b:
		name = "escape"
	case k.b == '\r' || k.b == '\n':
		if k.r == '\n' {
			return "" // decoded newline insert (alt+enter), not a keystroke
		}
		name = "enter"
	case k.b == '\t':
		name = "tab"
	case k.b == ' ':
		if mods == 0 {
			return ""
		}
		name = "space"
	case k.b == 0x7f || k.b == 8:
		name = "backspace"
	case k.b >= 1 && k.b <= 26:
		name = string(rune('a' + k.b - 1))
		mods |= modCtrl
	default:
		r := k.r
		if r == 0 && k.b >= 32 && k.b < 127 {
			r = rune(k.b)
		}
		if r == 0 || mods == 0 {
			return "" // plain printable text isn't a bindable stroke
		}
		if r == ' ' {
			name = "space"
		} else {
			if r >= 'A' && r <= 'Z' {
				r += 32
			}
			name = string(r)
		}
	}
	var sb strings.Builder
	if mods&modCtrl != 0 {
		sb.WriteString("ctrl+")
	}
	if mods&modAlt != 0 {
		sb.WriteString("alt+")
	}
	if mods&modShift != 0 {
		sb.WriteString("shift+")
	}
	if mods&modCmd != 0 {
		sb.WriteString("cmd+")
	}
	sb.WriteString(name)
	return sb.String()
}

// parseKeystroke canonicalizes one keystroke ("ctrl+shift+p"). Returns ""
// with a warning on invalid input. Non-modifier junk tokens are dropped with
// a warning, matching CC's validation message.
func parseKeystroke(s string) (string, []string) {
	var warns []string
	mods, name := 0, ""
	for _, tok := range strings.Split(s, "+") {
		t := strings.ToLower(strings.TrimSpace(tok))
		if t == "" {
			continue
		}
		if m, ok := kbModAliases[t]; ok {
			mods |= m
			continue
		}
		if kbSpecialKeys[t] || len([]rune(t)) == 1 || isFKey(t) {
			name = t
			continue
		}
		warns = append(warns, fmt.Sprintf("%q is not a modifier, so %q applies to %q instead", t, s, t))
	}
	if name == "" {
		warns = append(warns, fmt.Sprintf("invalid keystroke %q", s))
		return "", warns
	}
	var sb strings.Builder
	if mods&modCtrl != 0 {
		sb.WriteString("ctrl+")
	}
	if mods&modAlt != 0 {
		sb.WriteString("alt+")
	}
	if mods&modShift != 0 {
		sb.WriteString("shift+")
	}
	if mods&modCmd != 0 {
		sb.WriteString("cmd+")
	}
	sb.WriteString(name)
	return sb.String(), warns
}

func isFKey(s string) bool {
	if len(s) < 2 || s[0] != 'f' {
		return false
	}
	for _, ch := range s[1:] {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// parseChord canonicalizes a space-separated chord ("ctrl+x ctrl+k").
func parseChord(s string) (string, []string) {
	var warns []string
	var parts []string
	for _, tok := range strings.Fields(s) {
		cs, w := parseKeystroke(tok)
		warns = append(warns, w...)
		if cs == "" {
			return "", warns
		}
		parts = append(parts, cs)
	}
	return strings.Join(parts, " "), warns
}

// loadKeybindings builds the effective binding map: defaults first, then
// ~/.claude/keybindings.json applied over them (null unbinds).
func (e *editor) loadKeybindings() {
	e.kb = map[string]string{}
	e.kbPrefix = map[string]bool{}
	e.kbUnbound = map[string]bool{}
	var warns []string
	for _, d := range defaultKeybindings {
		cs, w := parseChord(d.chord)
		warns = append(warns, w...)
		if cs != "" {
			e.kb[cs] = d.action
		}
	}
	if b, err := os.ReadFile(filepath.Join(config.ClaudeDir(), "keybindings.json")); err == nil {
		warns = append(warns, e.applyKeybindingFile(b)...)
	}
	for cs := range e.kb {
		parts := strings.Split(cs, " ")
		for i := 1; i < len(parts); i++ {
			e.kbPrefix[strings.Join(parts[:i], " ")] = true
		}
	}
	if len(warns) > 0 {
		fmt.Fprintf(os.Stderr, "[keybindings] Found %d validation issue(s)\n", len(warns))
		for _, w := range warns {
			fmt.Fprintf(os.Stderr, "[keybindings] %s\n", w)
		}
	}
}

func (e *editor) applyKeybindingFile(b []byte) []string {
	var warns []string
	var file struct {
		Bindings []struct {
			Context  string             `json:"context"`
			Bindings map[string]*string `json:"bindings"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		return []string{fmt.Sprintf("invalid JSON in keybindings.json: %v", err)}
	}
	if file.Bindings == nil {
		return []string{`keybindings.json must have a "bindings" array`}
	}
	for _, cb := range file.Bindings {
		if cb.Context != "Chat" && cb.Context != "Global" {
			warns = append(warns, fmt.Sprintf("Unknown context %q — Valid contexts: Global, Chat", cb.Context))
			continue
		}
		for kstr, act := range cb.Bindings {
			cs, w := parseChord(kstr)
			warns = append(warns, w...)
			if cs == "" {
				continue
			}
			if act == nil {
				delete(e.kb, cs) // null unbinds the default
				e.kbUnbound[cs] = true
				continue
			}
			if _, ok := kbActions[*act]; !ok {
				warns = append(warns, fmt.Sprintf("Invalid action for %q: %q", kstr, *act))
				continue
			}
			delete(e.kbUnbound, cs)
			e.kb[cs] = *act
		}
	}
	return warns
}

// bindingKey dispatches a key through the keybinding layer. Returns
// (result, consumed); consumed=false means fall through to default handling.
func (e *editor) bindingKey(k key) (*readResult, bool) {
	if e.kb == nil {
		return nil, false
	}
	if len(e.pendingChord) > 0 && time.Since(e.chordAt) >= 3*time.Second {
		e.pendingChord = nil
	}
	s := keystroke(k)
	if s == "" {
		if len(e.pendingChord) > 0 {
			e.pendingChord = nil // non-stroke key breaks the chord
		}
		return nil, false
	}
	hadPending := len(e.pendingChord) > 0
	cand := s
	if hadPending {
		cand = strings.Join(append(append([]string{}, e.pendingChord...), s), " ")
	}
	e.pendingChord = nil
	if e.kbPrefix[cand] {
		e.pendingChord = strings.Fields(cand)
		e.chordAt = time.Now()
		return nil, true
	}
	if act, ok := e.kb[cand]; ok {
		if f := kbActions[act]; f != nil {
			return f(e), true
		}
		return nil, true
	}
	if e.kbUnbound[cand] {
		return nil, true
	}
	if hadPending {
		return e.bindingKey(k) // chord missed: retry stroke standalone
	}
	return nil, false
}
