package repl

import (
	"encoding/json"
	"fmt"
	"github.com/gbasin/claude-go/internal/config"
	"golang.org/x/term"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Clawd mascot + welcome banner — byte-for-byte port of the real welcome
// component (mod_1331.js, function GX; Apple Terminal variant is function V).
//
// Real gates on rows < 30 (z) or screen-reader → one-line text fallback.
// Otherwise TERM_PROGRAM=Apple_Terminal → V (draws clawd with inverse-video
// runs because Apple Terminal can't do fg/bg per half-glyph), themes
// light/light-daltonized/light-ansi → light art, else dark art.
//
// Escape codes match what Ink + the bundled chalk emit (mod_0267.js FUe):
//   fg rgb   \x1b[38;2;r;g;bm … \x1b[39m
//   bg rgb   \x1b[48;2;r;g;bm … \x1b[49m
//   dim      \x1b[2m … \x1b[22m
//   bold     \x1b[1m … \x1b[22m
// FUe applies color first, then backgroundColor wraps the result, so a
// fg+bg run is bgOpen + fgOpen + text + fgClose + bgClose.
// The planet, ground dots, haze and stars are NOT clawd_body — only the
// clawd runs get the claudeOrange; match the JSX, not intuition.

const bannerWidth = 58 // real: A=58
const bannerMinRows = 30

// welcomeMessage mirrors V's prop; GX always passes the default.
const defaultWelcomeMessage = "Welcome to Claude Code"

const (
	fgClose  = "\x1b[39m"
	bgClose  = "\x1b[49m"
	modClose = "\x1b[22m" // shared close for bold(1) and dim(2)
)

// bannerPalette is the per-theme resolution of the three theme tokens the
// banner uses (mod_0267.js):
//
//	theme            claude            clawd_body        clawd_background
//	dark, light      rgb(215,119,87)   rgb(215,119,87)   rgb(0,0,0)
//	*-daltonized     rgb(255,153,51)   rgb(215,119,87)   rgb(0,0,0)
//	*-ansi           ansi:redBright    ansi:redBright    ansi:black
//
// clawd_background is black on every theme — on light themes it paints the
// eye dips so they stay dark on a light terminal.
type bannerPalette struct {
	claude string // fg opener for the welcome text
	body   string // fg opener for clawd_body
	bodyBg string // bg opener for clawd_body (Apple variant)
	ink    string // fg opener for clawd_background (Apple ▗/▖ run)
	inkBg  string // bg opener for clawd_background (light-theme eyes)
	dim    string // \x1b[2m
	bold   string // \x1b[1m
}

func bannerPaletteFor(theme string) bannerPalette {
	p := bannerPalette{
		claude: "\x1b[38;2;215;119;87m",
		body:   "\x1b[38;2;215;119;87m",
		bodyBg: "\x1b[48;2;215;119;87m",
		ink:    "\x1b[38;2;0;0;0m",
		inkBg:  "\x1b[48;2;0;0;0m",
		dim:    "\x1b[2m",
		bold:   "\x1b[1m",
	}
	switch {
	case strings.HasSuffix(theme, "-ansi"):
		p.claude, p.body, p.bodyBg = "\x1b[91m", "\x1b[91m", "\x1b[101m"
		p.ink, p.inkBg = "\x1b[30m", "\x1b[40m"
	case strings.Contains(theme, "daltonized"):
		p.claude = "\x1b[38;2;255;153;51m"
	}
	return p
}

// bannerPaletteEmpty mirrors chalk level 0 (NO_COLOR): every run is a no-op.
var bannerPaletteEmpty bannerPalette

// When an opener is empty (chalk level 0 / NO_COLOR) the run is a no-op —
// real chalk emits nothing at all, not even close codes.
func (p bannerPalette) fg(open, s string) string {
	if open == "" {
		return s
	}
	return open + s + fgClose
}
func (p bannerPalette) bg(open, s string) string {
	if open == "" {
		return s
	}
	return open + s + bgClose
}
func (p bannerPalette) dimRun(s string) string {
	if p.dim == "" {
		return s
	}
	return p.dim + s + modClose
}
func (p bannerPalette) boldRun(s string) string {
	if p.bold == "" {
		return s
	}
	return p.bold + s + modClose
}
func (p bannerPalette) inkOnBody(s string) string {
	if p.bodyBg == "" {
		return s
	}
	return p.bodyBg + p.ink + s + fgClose + bgClose
}
func (p bannerPalette) bodyOnInk(s string) string {
	if p.inkBg == "" {
		return s
	}
	return p.inkBg + p.body + s + fgClose + bgClose
}

// resolveBannerTheme mirrors er()/auto behavior: empty/auto resolves via the
// terminal env; NO_COLOR (without FORCE_COLOR) and the none/no-color theme
// names collapse to the empty palette. Art selection still uses the resolved
// theme — chalk level only kills the escapes, not which art real picks.
func resolveBannerTheme(theme string) (string, bannerPalette) {
	if theme == "" || theme == "auto" {
		theme = autoTheme()
	}
	if theme == "none" || theme == "no-color" ||
		(os.Getenv("NO_COLOR") != "" && os.Getenv("FORCE_COLOR") == "") {
		return theme, bannerPaletteEmpty
	}
	return theme, bannerPaletteFor(theme)
}

func isLightBannerTheme(theme string) bool {
	switch theme {
	case "light", "light-daltonized", "light-ansi":
		return true
	}
	return false
}

// welcomeBanner prints the banner for the resolved variant. Signature is
// fixed by repl.go — version is the app version, rows the terminal height,
// theme the configured theme name, appleTerm TERM_PROGRAM==Apple_Terminal.
func welcomeBanner(version string, rows int, theme string, appleTerm bool) {
	resolved, p := resolveBannerTheme(theme)
	var lines []string
	// real also forces the text fallback in screen-reader mode
	// (CLAUDE_AX_SCREEN_READER or CLAUDE_CODE_ACCESSIBILITY — Xe()/Is)
	switch {
	case rows < bannerMinRows || screenReaderMode():
		lines = []string{bannerHeaderShort(defaultWelcomeMessage, version, p)}
	case appleTerm:
		lines = bannerRowsApple(resolved, defaultWelcomeMessage, version, p)
	case isLightBannerTheme(resolved):
		lines = bannerRowsLight(version, p)
	default:
		lines = bannerRowsDark(version, p)
	}
	for _, l := range lines {
		fmt.Println(l)
	}
}

// header used by every art variant: colored "Welcome to Claude Code " +
// dim "v<version> " (trailing space inside each run, per the JSX children).
func bannerHeader(msg, version string, p bannerPalette) string {
	return p.fg(p.claude, msg+" ") + p.dimRun("v"+version+" ")
}

// rows<30 / screen-reader fallback — same but the dim run has no trailing
// space (children are ["v", VERSION]).
func bannerHeaderShort(msg, version string, p bannerPalette) string {
	return p.fg(p.claude, msg+" ") + p.dimRun("v"+version)
}

var (
	bannerDots  = strings.Repeat(".", bannerWidth)
	bannerBlank = strings.Repeat(" ", bannerWidth)
)

// bannerRowsDark is the default art: starfield + planet (all plain text),
// dim ground haze, clawd in clawd_body.
func bannerRowsDark(version string, p bannerPalette) []string {
	return []string{
		bannerHeader(defaultWelcomeMessage, version, p),
		bannerDots,
		bannerBlank,
		"     *                                       █████▓▓░     ",
		"                                 *         ███▓░     ░░   ",
		"            ░░░░░░                        ███▓░           ",
		"    ░░░   ░░░░░░░░░░                      ███▓░           ",
		"   ░░░░░░░░░░░░░░░░░░░    " + p.boldRun("*") + "                ██▓░░      ▓   ",
		"                                             ░▓▓███▓▓░    ",
		p.dimRun(" *                                 ░░░░                   "),
		p.dimRun("                                 ░░░░░░░░                 "),
		p.dimRun("                               ░░░░░░░░░░░░░░░░           "),
		"      " + p.fg(p.body, " █████████ ") + "                                       " + p.dimRun("*") + " ",
		"      " + p.fg(p.body, "██▄█████▄██") + "                        " + p.boldRun("*") + "                ",
		"      " + p.fg(p.body, " █████████ ") + "     *                                   ",
		"......." + p.fg(p.body, "█ █   █ █") + "..........................................",
	}
}

// bannerRowsLight is the light-theme art: no planet, ░/▒ ground haze on the
// right, same clawd. Eye row carries backgroundColor=clawd_background so the
// ▄ dips stay dark on a light terminal.
func bannerRowsLight(version string, p bannerPalette) []string {
	return []string{
		bannerHeader(defaultWelcomeMessage, version, p),
		bannerDots,
		bannerBlank,
		bannerBlank,
		bannerBlank,
		"            ░░░░░░                                        ",
		"    ░░░   ░░░░░░░░░░                                      ",
		"   ░░░░░░░░░░░░░░░░░░░                                    ",
		bannerBlank,
		p.dimRun("                           ░░░░") + "                     ██    ",
		p.dimRun("                         ░░░░░░░░░░") + "               ██▒▒██  ",
		"                                            ▒▒      ██   ▒",
		"      " + p.fg(p.body, " █████████ ") + "                         ▒▒░░▒▒      ▒ ▒▒",
		"      " + p.bodyOnInk("██▄█████▄██") + "                           ▒▒         ▒▒ ",
		"      " + p.fg(p.body, " █████████ ") + "                          ░          ▒   ",
		"......." + p.fg(p.body, "█ █   █ █") + "..........................░..........▒....",
	}
}

// appleClawdHead is V's fg/bg-swap clawd head: ▗ and ▖ in clawd_body around
// a clawd_background-on-clawd_body run " ▗     ▖ " (JSX children
// [" ","▗","     ","▖"," "]).
func appleClawdHead(p bannerPalette) string {
	return "      " + p.fg(p.body, "▗") + p.inkOnBody(" ▗     ▖ ") + p.fg(p.body, "▖")
}

// appleClawdLegs is V's legs row body: alternating bg-colored and plain
// single spaces inside a dotted line.
func appleClawdLegs(p bannerPalette) string {
	return "......." +
		p.bg(p.bodyBg, " ") + " " +
		p.bg(p.bodyBg, " ") + "   " +
		p.bg(p.bodyBg, " ") + " " +
		p.bg(p.bodyBg, " ")
}

// bannerRowsApple is component V: same scene, but clawd is drawn with
// background runs (Apple Terminal can't show fg/bg split glyphs cleanly).
// Theme picks the light or dark scene exactly like GX.
func bannerRowsApple(theme, welcomeMessage, version string, p bannerPalette) []string {
	head := bannerHeader(welcomeMessage, version, p)
	legs := appleClawdLegs(p)
	if isLightBannerTheme(theme) {
		return []string{
			head,
			bannerDots,
			bannerBlank,
			bannerBlank,
			bannerBlank,
			"            ░░░░░░                                        ",
			"    ░░░   ░░░░░░░░░░                                      ",
			"   ░░░░░░░░░░░░░░░░░░░                                    ",
			bannerBlank,
			p.dimRun("                           ░░░░") + "                     ██    ",
			p.dimRun("                         ░░░░░░░░░░") + "               ██▒▒██  ",
			"                                            ▒▒      ██   ▒",
			"                                          ▒▒░░▒▒      ▒ ▒▒",
			appleClawdHead(p) + "                           ▒▒         ▒▒ ",
			"       " + p.bg(p.bodyBg, strings.Repeat(" ", 9)) + "                           ░          ▒   ",
			legs + "..........................░..........▒....",
		}
	}
	return []string{
		head,
		bannerDots,
		bannerBlank,
		"     *                                       █████▓▓░     ",
		"                                 *         ███▓░     ░░   ",
		"            ░░░░░░                        ███▓░           ",
		"    ░░░   ░░░░░░░░░░                      ███▓░           ",
		"   ░░░░░░░░░░░░░░░░░░░    " + p.boldRun("*") + "                ██▓░░      ▓   ",
		"                                             ░▓▓███▓▓░    ",
		p.dimRun(" *                                 ░░░░                   "),
		p.dimRun("                                 ░░░░░░░░                 "),
		p.dimRun("                               ░░░░░░░░░░░░░░░░           "),
		"                                                      " + p.dimRun("*") + "   ",
		appleClawdHead(p) + "                       " + p.boldRun("*") + "                 ",
		"       " + p.bg(p.bodyBg, strings.Repeat(" ", 9)) + "      *                                   ",
		legs + "..........................................",
	}
}

// screenReaderMode mirrors Xe(): CLAUDE_AX_SCREEN_READER or
// CLAUDE_CODE_ACCESSIBILITY forces text-only output.
func screenReaderMode() bool {
	return os.Getenv("CLAUDE_AX_SCREEN_READER") != "" ||
		os.Getenv("CLAUDE_CODE_ACCESSIBILITY") != ""
}

// compactBanner — real 2.1.289's session-start banner at every width
// (captured live: 190x50 and 80x40 panes both render the 3-line mascot,
// never the starfield — the tall art is a first-run path).
//
//	 ▐▛███▛█   Claude Code v2.1.289
//	▝▜██████▀  Fable 5.1 with medium effort · Claude Max
//	 ▝▝   ▝▝   ~/Code · /rc            (/rc is an OSC8 session link)
//
// mascotRows is real's 3-line Clawd block art (the compact banner form).
var mascotRows = [3]string{" \u2590\u259b\u2588\u2588\u2588\u259b\u2588", "\u259d\u259c\u2588\u2588\u2588\u2588\u2588\u2588\u2580", " \u259d\u259d   \u259d\u259d"}

// animateMascot paints the mascot cell-by-cell — real's startup does a
// progressive sparkle fill (each frame reveals scattered block cells).
// Skipped under prefers-reduced-motion or non-TTY.
func animateMascot() {
	if !term.IsTerminal(int(os.Stdout.Fd())) || os.Getenv("CLAUDE_GO_REDUCED_MOTION") != "" || reducedMotionSetting() {
		return
	}
	type cell struct {
		r, col int // visual row + cell column
		ch     rune
	}
	var cells []cell
	for r := 0; r < 3; r++ {
		col := 0
		for _, ch := range mascotRows[r] {
			if ch != ' ' {
				cells = append(cells, cell{r, col, ch})
			}
			col++
		}
	}
	var state uint64 = 0x9e3779b97f4a7c15
	for i := len(cells) - 1; i > 0; i-- {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		j := int(state % uint64(i+1))
		cells[i], cells[j] = cells[j], cells[i]
	}
	if !envOn("CLAUDE_CODE_NATIVE_CURSOR") {
		fmt.Print("\x1b[?25l") // real draws its own cursor unless the env asks for native
	}
	for i, cl := range cells {
		fmt.Printf("\x1b[%d;1H\x1b[%dC%s%c%s", cl.r+1, cl.col, claudeOrange, cl.ch, reset)
		if i%5 == 0 {
			time.Sleep(25 * time.Millisecond)
		}
	}
	time.Sleep(30 * time.Millisecond)
	// repaint cleanly — the scatter left the cursor mid-screen
	fmt.Print("\x1b[1;1H\x1b[3B")
}

func compactBanner(version, name, effort, plan, cwd, sessionID string) {
	home, _ := os.UserHomeDir()
	short := cwd
	if home != "" && strings.HasPrefix(cwd, home) {
		short = "~" + cwd[len(home):]
	}
	if envOn("CLAUDE_CODE_HIDE_CWD") {
		short = "" // real: the cwd label is suppressed entirely
	}
	link := fmt.Sprintf("\x1b]8;id=%x;https://claude.ai/code/session_%s?from=cli\x07/rc\x1b]8;;\x07",
		uint32(time.Now().UnixNano()&0xffffffff), sessionID)
	// \r\n — the editor is in raw mode when the banner prints (ONLCR off,
	// bare \n staircases). Reserve the three mascot rows, sparkle-fill,
	// then write the header text beside them (real's start-up animation).
	fmt.Print("\r\n\r\n\r\n\x1b[3A")
	animateMascot()
	fmt.Printf(" %s▐▛███▛█%s   %sClaude Code%s %sv%s%s\r\n", claudeOrange, reset, bold, reset, dim, version, reset)
	if effort != "" {
		fmt.Printf("%s▝▜██████▀%s  %s with %s effort · %s\r\n", claudeOrange, reset, name, effort, plan)
	} else {
		fmt.Printf("%s▝▜██████▀%s  %s · %s\r\n", claudeOrange, reset, name, plan)
	}
	if short != "" {
		fmt.Printf(" %s▝▝   ▝▝%s   %s · %s\r\n", claudeOrange, reset, short, link)
	} else {
		fmt.Printf(" %s▝▝   ▝▝%s   %s\r\n", claudeOrange, reset, link)
	}
}

// printAutoModeNotice renders real's one-time auto-mode announcement —
// a pink ▎ quote block with the permission-modes link. Shown once per
// install (tracked via ~/.claude.json autoModeNoticeShown).
func printAutoModeNotice(permMode string) {
	if permMode != "auto" {
		return
	}
	m, _ := config.ReadClaudeJSON()
	if shown, _ := m["autoModeNoticeShown"].(bool); shown {
		return
	}
	pink := "\x1b[38;5;174m"
	gray := "\x1b[38;5;246m"
	fmt.Printf("%s▎%s  %sAuto mode is now Claude Code's default permission mode.%s\n",
		pink, reset, bold, "\x1b[22m"+reset)
	for _, ln := range []string{
		"Auto mode lets Claude handle permission prompts automatically. Claude checks",
		"each tool call for risky actions and prompt injection before executing, runs",
		"the ones it assesses as lower-risk, and blocks the rest.",
	} {
		fmt.Printf("%s▎%s  %s%s%s\n", pink, reset, gray, ln, reset)
	}
	fmt.Printf("%s▎%s  %s\x1b]8;;https://code.claude.com/docs/en/permission-modes\x07%shttps://code.claude.com/docs/en/permission-modes%s\x1b]8;;\x07%s\n",
		pink, reset, gray, gray, reset, reset)
	m["autoModeNoticeShown"] = true
	if b, err := json.MarshalIndent(m, "", "  "); err == nil {
		os.WriteFile(filepath.Join(config.Home(), ".claude.json"), b, 0o644)
	}
}

// reducedMotionSetting honors settings.prefersReducedMotion (real's key).
func reducedMotionSetting() bool {
	v, _ := config.UserSetting("prefersReducedMotion").(bool)
	return v
}
