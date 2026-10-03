package repl

import (
	"fmt"
	"os"
	"strings"

	"claude-go/internal/session"
	"golang.org/x/term"
)

// PickSession shows the interactive session resume list (like `claude -r`).
// Returns the chosen session id, or "" to start fresh / quit.
func PickSession(cwd string) string {
	infos, err := session.List(cwd)
	if err != nil || len(infos) == 0 {
		fmt.Println("  no sessions found for this directory")
		return ""
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		session.Print(cwd)
		return ""
	}
	ks, err := startKeys()
	if err != nil {
		session.Print(cwd)
		return ""
	}
	defer ks.stop()

	sel := 0
	draw := func() {
		fmt.Print("\r\x1b[J")
		fmt.Printf("%sResume session (enter to select, esc to cancel)%s\r\n", bold, reset)
		max := len(infos)
		if max > 15 {
			max = 15
		}
		for i := 0; i < max; i++ {
			in := infos[i]
			pv := in.Preview
			if len(pv) > 60 {
				pv = pv[:60] + "…"
			}
			line := fmt.Sprintf("  %s  %s", in.Modified.Format("2006-01-02 15:04"), pv)
			if i == sel {
				fmt.Printf("%s❯%s%s%s\r\n", cyan, reset, line, strings.Repeat(" ", 2))
			} else {
				fmt.Printf(" %s%s%s\r\n", dim, line, reset)
			}
		}
	}
	rows := func() int {
		n := len(infos)
		if n > 15 {
			n = 15
		}
		return n + 1
	}
	draw()
	for k := range ks.ch {
		switch {
		case k.seq == "[A":
			if sel > 0 {
				sel--
			}
		case k.seq == "[B":
			if sel < len(infos)-1 {
				sel++
			}
		case k.b == '\r':
			return infos[sel].ID
		case k.b == 0x1b || k.b == 3:
			fmt.Print("\r\n")
			return ""
		case k.b == 'j':
			if sel < len(infos)-1 {
				sel++
			}
		case k.b == 'k':
			if sel > 0 {
				sel--
			}
		}
		// redraw: move up rows, clear, reprint
		fmt.Printf("\x1b[%dA", rows())
		draw()
	}
	return ""
}
