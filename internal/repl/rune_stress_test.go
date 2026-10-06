package repl

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// fuzz the width machinery: compClamp + softWrap over hostile input at
// every width — any byte-sliced rune fails utf8 validity.
func TestNoRuneSplitting(t *testing.T) {
	inputs := []string{
		"emoji 👨\u200d👩\u200d👧 tail",
		"🇯🇵 flags and 中文字符 mixed \x1b[31mred\x1b[39m",
		strings.Repeat("✻", 60),
		"combining a\u0301b\u0302 and ZWJ seq",
		"skin 👍🏿 tone + v16 ❤️",
		"edge\x1b[38;5;153m✻⏺❯◐⏵⏵\x1b[39mtail",
		"┌─box─drawing─┐ │ chars",
	}
	for w := 1; w <= 80; w++ {
		for _, in := range inputs {
			if got := compClamp(in, w); !utf8.ValidString(got) {
				t.Fatalf("compClamp(%d) split a rune: %q", w, got)
			}
			for _, row := range softWrap(in, w) {
				if !utf8.ValidString(row) {
					t.Fatalf("softWrap(%d) split a rune: %q", w, row)
				}
			}
		}
	}
}
