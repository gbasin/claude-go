package repl

import (
	"os"
	"strings"
)

// mdRenderer incrementally renders streamed markdown to ANSI.
// Inline: `code` -> cyan, **bold** -> bold, *italic* -> italic.
// Blocks: # headers -> bold, ``` fence -> dim, "- " -> bullet.
type mdRenderer struct {
	lineStart  bool
	fence      bool
	inlineCode bool
	bold       bool
	italic     bool
	lang       string
	pending    strings.Builder // chars held while we disambiguate
}

func newMDRenderer() *mdRenderer {
	return &mdRenderer{lineStart: true}
}

func (r *mdRenderer) Write(s string) {
	r.pending.WriteString(s)
	r.flush(false)
}

// Close flushes any held formatting chars.
func (r *mdRenderer) Close() { r.flush(true) }

func (r *mdRenderer) flush(eof bool) {
	s := r.pending.String()
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '\n' {
			os.Stdout.WriteString(reset + "\n")
			r.lineStart = true
			r.inlineCode, r.bold, r.italic = false, false, false
			i++
			continue
		}
		if r.lineStart {
			// check block markers
			rest := s[i:]
			if strings.HasPrefix(rest, "```") {
				r.fence = !r.fence
				j := i + 3
				for j < len(s) && s[j] != '\n' {
					j++
				}
				i = j
				if r.fence {
					os.Stdout.WriteString(dim)
				} else {
					os.Stdout.WriteString(reset)
				}
				continue
			}
			if !r.fence {
				if strings.HasPrefix(rest, "#") {
					j := i
					for j < len(s) && s[j] == '#' {
						j++
					}
					if j < len(s) && s[j] == ' ' {
						os.Stdout.WriteString(bold)
						i = j + 1
						r.lineStart = false
						continue
					}
				}
				if strings.HasPrefix(rest, "- ") || strings.HasPrefix(rest, "* ") {
					os.Stdout.WriteString("  " + cyan + "•" + reset + " ")
					i += 2
					r.lineStart = false
					continue
				}
				if strings.HasPrefix(rest, "> ") {
					os.Stdout.WriteString(dim + "▎" + reset)
					i += 2
					r.lineStart = false
					continue
				}
			}
			r.lineStart = false
		}
		if r.fence {
			// verbatim
			j := i
			for j < len(s) && s[j] != '\n' {
				j++
			}
			os.Stdout.WriteString(s[i:j])
			i = j
			continue
		}
		// inline formatting
		if strings.HasPrefix(s[i:], "**") {
			r.bold = !r.bold
			if r.bold {
				os.Stdout.WriteString(bold)
			} else {
				os.Stdout.WriteString(reset)
			}
			i += 2
			continue
		}
		if s[i] == '`' {
			r.inlineCode = !r.inlineCode
			if r.inlineCode {
				os.Stdout.WriteString(cyan)
			} else {
				os.Stdout.WriteString(reset)
			}
			i++
			continue
		}
		if s[i] == '*' && !strings.HasPrefix(s[i:], "**") {
			// could be italic marker; hold if at end of buffer
			if i == len(s)-1 && !eof {
				r.pending.Reset()
				r.pending.WriteString(s[i:])
				return
			}
			r.italic = !r.italic
			if r.italic {
				os.Stdout.WriteString("\x1b[3m")
			} else {
				os.Stdout.WriteString(reset)
			}
			i++
			continue
		}
		os.Stdout.WriteString(string(c))
		i++
	}
	r.pending.Reset()
}
