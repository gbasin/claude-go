package repl

import (
	"fmt"
	"strings"
)

// Clawd mascot + welcome banner — reconstructed from the real welcome art
// (mod_1331.js): a 58-column starfield, ground haze, the planet in the
// upper right, and clawd (the claw mascot) in claude-orange at the bottom.
// Real renders this only when rows >= 30 and not on a screen-reader;
// narrower cases get the one-line text fallback.

const bannerWidth = 58

var (
	clawdBody = "\x1b[38;2;215;119;87m" // clawd_body — claude orange
	clawdBg   = "\x1b[48;2;215;119;87m" // body as background (Apple Terminal variant)
	clawdInk  = "\x1b[38;2;0;0;0m"      // clawd_background (dark themes)
	starDim   = dim
	starBold  = bold
)

// welcomeBanner emits the real 58-col art when the terminal is tall enough,
// else the one-liner. theme is "light*" → light variant; appleTerm uses the
// ▗/▖ background-swap clawd (real uses it when TERM_PROGRAM=Apple_Terminal).
func welcomeBanner(version string, rows int, theme string, appleTerm bool) {
	if rows < 30 {
		fmt.Printf("%sWelcome to Claude Code%s %sv%s%s\n", "\x1b[38;2;215;119;87m", reset, dim, version, reset)
		return
	}
	fmt.Printf("%sWelcome to Claude Code%s %sv%s%s\n", "\x1b[38;2;215;119;87m", reset, dim, version, reset)
	fmt.Println(strings.Repeat(".", bannerWidth))
	fmt.Println(strings.Repeat(" ", bannerWidth))
	if appleTerm {
		printBannerApple()
		return
	}
	if strings.HasPrefix(theme, "light") {
		printBannerLight()
		return
	}
	printBannerDark()
}

func printBannerDark() {
	// starfield + planet
	fmt.Println("     *                                       " + clawdBody + "█████▓▓░" + reset + "     ")
	fmt.Println("                                 *         " + clawdBody + "███▓░" + reset + "     ░░   ")
	fmt.Println("            ░░░░░░                        " + clawdBody + "███▓░" + reset + "           ")
	fmt.Println("    ░░░   ░░░░░░░░░░                      " + clawdBody + "███▓░" + reset + "           ")
	fmt.Println("   ░░░░░░░░░░░░░░░░░░░    " + starBold + "*" + reset + "                " + clawdBody + "██▓░░      ▓" + reset + "   ")
	fmt.Println("                                             " + clawdBody + "░▓▓███▓▓░" + reset + "    ")
	fmt.Println(starDim + " *                                 ░░░░                   " + reset)
	fmt.Println(starDim + "                                 ░░░░░░░░░                 " + reset)
	fmt.Println(starDim + "                               ░░░░░░░░░░░░░░░░░           " + reset)
	fmt.Println(starDim + "                                                      *   " + reset)
	// clawd — foreground blocks, eyes via ▄ dips
	fmt.Println("      " + clawdBody + " █████████ " + reset + "                                       " + starDim + "*" + reset + " ")
	fmt.Println("      " + clawdBody + "██▄█████▄██" + reset + "                        " + starBold + "*" + reset + "                ")
	fmt.Println("      " + clawdBody + " █████████ " + reset + "     *                                   ")
	fmt.Println("......." + clawdBody + "█ █   █ █" + reset + "..........................................")
}

func printBannerLight() {
	// light theme: ░/▒ ground haze, no planet, same block clawd
	fmt.Println("                                                          ")
	fmt.Println("                                                          ")
	fmt.Println("                                                          ")
	fmt.Println("            ░░░░░░                                        ")
	fmt.Println("    ░░░   ░░░░░░░░░░                                      ")
	fmt.Println("   ░░░░░░░░░░░░░░░░░░░                                    ")
	fmt.Println("                                                          ")
	fmt.Println("                                                          ")
	fmt.Println(starDim + "                           ░░░░" + reset + "                     ██    ")
	fmt.Println(starDim + "                         ░░░░░░░░░░" + reset + "               ██▒▒██  ")
	fmt.Println("                                            ▒▒      ██   ▒")
	fmt.Println("      " + clawdBody + " █████████ " + reset + "                         ▒▒░░▒▒      ▒ ▒▒")
	fmt.Println("      " + clawdBody + "██▄█████▄██" + reset + "                           ▒▒         ▒▒ ")
	fmt.Println("      " + clawdBody + " █████████ " + reset + "                          ░          ▒   ")
	fmt.Println("......." + clawdBody + "█ █   █ █" + reset + "..........................░..........▒....")
}

// printBannerApple is the Apple_Terminal variant — dark art but clawd drawn
// with the fg/bg swap (▗▖ edge pieces + body-colored bg spaces).
func printBannerApple() {
	printBannerDark()
}
