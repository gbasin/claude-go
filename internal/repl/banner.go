package repl

import (
	"fmt"
	"os"
	"strings"
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
// clawd runs get the orange; match the JSX, not intuition.

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
	switch {
	case rows < bannerMinRows:
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
