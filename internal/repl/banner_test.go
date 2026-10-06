package repl

import (
	"io"
	"os"
	"strings"
	"testing"
)

// captureBanner runs f with stdout redirected to a pipe, same pattern as
// renderToString in render_test.go.
func captureBanner(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	f()
	os.Stdout = old
	w.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

// clearColorEnv keeps NO_COLOR/FORCE_COLOR from leaking into tests that
// assert on escape codes (CI runners sometimes set them).
func clearColorEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NO_COLOR", "")
	t.Setenv("FORCE_COLOR", "")
}

// Expected outputs below were generated from mod_1331.js (function GX and
// the Apple_Terminal component V) with chalk-style escapes per mod_0267.js
// FUe — NOT from this Go implementation.

func TestBannerDark(t *testing.T) {
	clearColorEnv(t)
	got := captureBanner(t, func() { welcomeBanner("2.1.292", 40, "dark", false) })
	want := "\x1b[38;2;215;119;87mWelcome to Claude Code \x1b[39m\x1b[2mv2.1.292 \x1b[22m" + "\n" +
		".........................................................." + "\n" +
		"                                                          " + "\n" +
		"     *                                       █████▓▓░     " + "\n" +
		"                                 *         ███▓░     ░░   " + "\n" +
		"            ░░░░░░                        ███▓░           " + "\n" +
		"    ░░░   ░░░░░░░░░░                      ███▓░           " + "\n" +
		"   ░░░░░░░░░░░░░░░░░░░    \x1b[1m*\x1b[22m                ██▓░░      ▓   " + "\n" +
		"                                             ░▓▓███▓▓░    " + "\n" +
		"\x1b[2m *                                 ░░░░                   \x1b[22m" + "\n" +
		"\x1b[2m                                 ░░░░░░░░                 \x1b[22m" + "\n" +
		"\x1b[2m                               ░░░░░░░░░░░░░░░░           \x1b[22m" + "\n" +
		"      \x1b[38;2;215;119;87m █████████ \x1b[39m                                       \x1b[2m*\x1b[22m " + "\n" +
		"      \x1b[38;2;215;119;87m██▄█████▄██\x1b[39m                        \x1b[1m*\x1b[22m                " + "\n" +
		"      \x1b[38;2;215;119;87m █████████ \x1b[39m     *                                   " + "\n" +
		".......\x1b[38;2;215;119;87m█ █   █ █\x1b[39m.........................................." + "\n"
	if got != want {
		t.Fatalf("dark banner mismatch:\ngot  %q\nwant %q", got, want)
	}
}

func TestBannerLight(t *testing.T) {
	clearColorEnv(t)
	got := captureBanner(t, func() { welcomeBanner("2.1.292", 40, "light", false) })
	want := "\x1b[38;2;215;119;87mWelcome to Claude Code \x1b[39m\x1b[2mv2.1.292 \x1b[22m" + "\n" +
		".........................................................." + "\n" +
		"                                                          " + "\n" +
		"                                                          " + "\n" +
		"                                                          " + "\n" +
		"            ░░░░░░                                        " + "\n" +
		"    ░░░   ░░░░░░░░░░                                      " + "\n" +
		"   ░░░░░░░░░░░░░░░░░░░                                    " + "\n" +
		"                                                          " + "\n" +
		"\x1b[2m                           ░░░░\x1b[22m                     ██    " + "\n" +
		"\x1b[2m                         ░░░░░░░░░░\x1b[22m               ██▒▒██  " + "\n" +
		"                                            ▒▒      ██   ▒" + "\n" +
		"      \x1b[38;2;215;119;87m █████████ \x1b[39m                         ▒▒░░▒▒      ▒ ▒▒" + "\n" +
		"      \x1b[48;2;0;0;0m\x1b[38;2;215;119;87m██▄█████▄██\x1b[39m\x1b[49m                           ▒▒         ▒▒ " + "\n" +
		"      \x1b[38;2;215;119;87m █████████ \x1b[39m                          ░          ▒   " + "\n" +
		".......\x1b[38;2;215;119;87m█ █   █ █\x1b[39m..........................░..........▒...." + "\n"
	if got != want {
		t.Fatalf("light banner mismatch:\ngot  %q\nwant %q", got, want)
	}
}

func TestBannerAppleDark(t *testing.T) {
	clearColorEnv(t)
	got := captureBanner(t, func() { welcomeBanner("2.1.292", 40, "dark", true) })
	want := "\x1b[38;2;215;119;87mWelcome to Claude Code \x1b[39m\x1b[2mv2.1.292 \x1b[22m" + "\n" +
		".........................................................." + "\n" +
		"                                                          " + "\n" +
		"     *                                       █████▓▓░     " + "\n" +
		"                                 *         ███▓░     ░░   " + "\n" +
		"            ░░░░░░                        ███▓░           " + "\n" +
		"    ░░░   ░░░░░░░░░░                      ███▓░           " + "\n" +
		"   ░░░░░░░░░░░░░░░░░░░    \x1b[1m*\x1b[22m                ██▓░░      ▓   " + "\n" +
		"                                             ░▓▓███▓▓░    " + "\n" +
		"\x1b[2m *                                 ░░░░                   \x1b[22m" + "\n" +
		"\x1b[2m                                 ░░░░░░░░                 \x1b[22m" + "\n" +
		"\x1b[2m                               ░░░░░░░░░░░░░░░░           \x1b[22m" + "\n" +
		"                                                      \x1b[2m*\x1b[22m   " + "\n" +
		"      \x1b[38;2;215;119;87m▗\x1b[39m\x1b[48;2;215;119;87m\x1b[38;2;0;0;0m ▗     ▖ \x1b[39m\x1b[49m\x1b[38;2;215;119;87m▖\x1b[39m                       \x1b[1m*\x1b[22m                 " + "\n" +
		"       \x1b[48;2;215;119;87m         \x1b[49m      *                                   " + "\n" +
		".......\x1b[48;2;215;119;87m \x1b[49m \x1b[48;2;215;119;87m \x1b[49m   \x1b[48;2;215;119;87m \x1b[49m \x1b[48;2;215;119;87m \x1b[49m.........................................." + "\n"
	if got != want {
		t.Fatalf("apple dark banner mismatch:\ngot  %q\nwant %q", got, want)
	}
}

func TestBannerAppleLight(t *testing.T) {
	clearColorEnv(t)
	got := captureBanner(t, func() { welcomeBanner("2.1.292", 40, "light", true) })
	want := "\x1b[38;2;215;119;87mWelcome to Claude Code \x1b[39m\x1b[2mv2.1.292 \x1b[22m" + "\n" +
		".........................................................." + "\n" +
		"                                                          " + "\n" +
		"                                                          " + "\n" +
		"                                                          " + "\n" +
		"            ░░░░░░                                        " + "\n" +
		"    ░░░   ░░░░░░░░░░                                      " + "\n" +
		"   ░░░░░░░░░░░░░░░░░░░                                    " + "\n" +
		"                                                          " + "\n" +
		"\x1b[2m                           ░░░░\x1b[22m                     ██    " + "\n" +
		"\x1b[2m                         ░░░░░░░░░░\x1b[22m               ██▒▒██  " + "\n" +
		"                                            ▒▒      ██   ▒" + "\n" +
		"                                          ▒▒░░▒▒      ▒ ▒▒" + "\n" +
		"      \x1b[38;2;215;119;87m▗\x1b[39m\x1b[48;2;215;119;87m\x1b[38;2;0;0;0m ▗     ▖ \x1b[39m\x1b[49m\x1b[38;2;215;119;87m▖\x1b[39m                           ▒▒         ▒▒ " + "\n" +
		"       \x1b[48;2;215;119;87m         \x1b[49m                           ░          ▒   " + "\n" +
		".......\x1b[48;2;215;119;87m \x1b[49m \x1b[48;2;215;119;87m \x1b[49m   \x1b[48;2;215;119;87m \x1b[49m \x1b[48;2;215;119;87m \x1b[49m..........................░..........▒...." + "\n"
	if got != want {
		t.Fatalf("apple light banner mismatch:\ngot  %q\nwant %q", got, want)
	}
}

// rows < 30 (real: N < z where z=30) → text-only fallback. Same shape as the
// screen-reader path. Note: dim run here has NO trailing space — real's
// children are ["v", VERSION], not ["v", VERSION, " "].
func TestBannerFallback(t *testing.T) {
	clearColorEnv(t)
	got := captureBanner(t, func() { welcomeBanner("2.1.292", 20, "dark", false) })
	want := "\x1b[38;2;215;119;87mWelcome to Claude Code \x1b[39m\x1b[2mv2.1.292\x1b[22m" + "\n"
	if got != want {
		t.Fatalf("fallback mismatch:\ngot  %q\nwant %q", got, want)
	}
}

// Boundary: exactly 30 rows still gets the full art (gate is N < 30).
func TestBannerRowsBoundary(t *testing.T) {
	clearColorEnv(t)
	at30 := captureBanner(t, func() { welcomeBanner("2.1.292", 30, "dark", false) })
	if !strings.Contains(at30, "█████▓▓░") {
		t.Fatalf("rows=30 should render full art, got %q", at30)
	}
	at29 := captureBanner(t, func() { welcomeBanner("2.1.292", 29, "dark", false) })
	if strings.Contains(at29, "█") {
		t.Fatalf("rows=29 should be text fallback, got %q", at29)
	}
}

// Theme-name → palette/art mapping (mod_0267 theme objects + GX's art list).
func TestBannerThemeVariants(t *testing.T) {
	clearColorEnv(t)
	cases := []struct {
		name     string
		theme    string
		contains []string
	}{
		{"light-daltonized picks light art + dalt claude", "light-daltonized",
			[]string{"\x1b[38;2;255;153;51mWelcome", "██▒▒██"}},
		{"dark-daltonized picks dark art + dalt claude", "dark-daltonized",
			[]string{"\x1b[38;2;255;153;51mWelcome", "█████▓▓░"}},
		{"light-ansi picks light art + ansi colors", "light-ansi",
			[]string{"\x1b[91mWelcome", "\x1b[91m █████████ \x1b[39m", "██▒▒██"}},
		{"dark-ansi picks dark art + ansi colors", "dark-ansi",
			[]string{"\x1b[91mWelcome", "█████▓▓░"}},
	}
	for _, tc := range cases {
		got := captureBanner(t, func() { welcomeBanner("2.1.292", 40, tc.theme, false) })
		for _, want := range tc.contains {
			if !strings.Contains(got, want) {
				t.Fatalf("%s: missing %q in %q", tc.name, want, got)
			}
		}
	}
}

// NO_COLOR → chalk level 0 → zero escape bytes (real emits the same art,
// just unstyled).
func TestBannerNoColor(t *testing.T) {
	t.Setenv("FORCE_COLOR", "")
	t.Setenv("NO_COLOR", "1")
	got := captureBanner(t, func() { welcomeBanner("2.1.292", 40, "dark", false) })
	if strings.ContainsRune(got, 0x1b) {
		t.Fatalf("NO_COLOR output contains escapes: %q", got)
	}
	if !strings.Contains(got, "Welcome to Claude Code v2.1.292") || !strings.Contains(got, "█████▓▓░") {
		t.Fatalf("NO_COLOR output missing art: %q", got)
	}
}
