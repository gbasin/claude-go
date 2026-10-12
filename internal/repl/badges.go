package repl

// Mode-badge rendering for the composer's #/! prefixes and the rows a
// submitted #/! line leaves in the transcript — audits/tui.md §66-67.
//
// Real colors (mod_1522's user-memory-input + bash-input renders, and
// the editor's input-time highlight span):
//   '#' → fg "remember" on bg "memoryBackgroundColor"
//   '!' → fg "bashBorder" (real strips '!' into bash mode and paints
//         the '!' prompt glyph + composer border in bashBorder)
// Submitted "# text" → '#' remember/memBG + " text " text/memBG, then a
// dim acknowledgement ("Got it." / "Good to know." / "Noted.").
// Submitted "!cmd"   → "! " bashBorder + cmd in "text", the row on
// bashMessageBackgroundColor.

import (
	"math/rand"
	"strings"
)

// tokenBG converts a palette foreground SGR into the matching
// background sequence. The palette tables (palettes_gen.go) store every
// token as a foreground escape — including the *BackgroundColor tokens
// real uses as badge backgrounds — so derive the bg form at render time.
func tokenBG(k string) string {
	fg := token(k)
	switch {
	case strings.HasPrefix(fg, "\x1b[38;"):
		return "\x1b[48;" + strings.TrimPrefix(fg, "\x1b[38;")
	case len(fg) == 5 && strings.HasPrefix(fg, "\x1b[3"):
		return "\x1b[4" + fg[3:] // ansi 30-37 -> 40-47
	case len(fg) == 5 && strings.HasPrefix(fg, "\x1b[9"):
		return "\x1b[10" + fg[3:] // ansi 90-97 -> 100-107
	}
	return ""
}

// modeBadgeRender paints the buffer's leading mode character with
// real's badge colors. Only the mode char is styled — the rest of the
// buffer stays default-text (real's highlight span covers just the '#').
//
// NOTE (editor-agent): real removes '!' from the buffer entirely and
// flips the input to bash mode — '!' becomes the prompt glyph in
// bashBorder and the composer border turns bashBorder (mod_1596
// Rzt/getInputMode). We keep '!' in the buffer, so this is the closest
// render; a true mode split needs the prompt-glyph + border-color hook
// in term.go's redraw.
func modeBadgeRender(buf string) string {
	if buf == "" {
		return buf
	}
	switch buf[0] {
	case '#':
		fg := token("remember")
		if fg == "" {
			fg = "\x1b[38;2;255;153;51m"
		}
		return tokenBG("memoryBackgroundColor") + fg + "#" + reset + buf[1:]
	case '!':
		fg := token("bashBorder")
		if fg == "" {
			fg = "\x1b[95m"
		}
		return fg + "!" + reset + buf[1:]
	}
	return buf
}

// memoryAcks — real's rotating acknowledgement line under a saved
// memory row (mod_1522 ll(): xU over this exact pool).
var memoryAcks = []string{"Got it.", "Good to know.", "Noted."}

// memoryInputEcho is the transcript row real leaves for a submitted
// "# text" — '#' in remember on memoryBackgroundColor, " text " in
// text on the same background, then a dim acknowledgement line.
func memoryInputEcho(text string) string {
	bg := tokenBG("memoryBackgroundColor")
	fg := token("remember")
	if fg == "" {
		fg = "\x1b[38;2;255;153;51m"
	}
	tx := token("text")
	return fg + bg + "#" + reset +
		bg + tx + " " + text + " " + reset + "\n" +
		dim + memoryAcks[rand.Intn(len(memoryAcks))] + reset
}

// bashInputEcho is the transcript row real leaves for a submitted
// "!cmd" — "! " in bashBorder + the command in text, the whole row on
// bashMessageBackgroundColor with one cell of right padding.
func bashInputEcho(cmd string) string {
	bg := tokenBG("bashMessageBackgroundColor")
	fg := token("bashBorder")
	if fg == "" {
		fg = "\x1b[95m"
	}
	tx := token("text")
	return bg + fg + "! " + reset + bg + tx + cmd + " " + reset
}
