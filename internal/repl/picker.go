package repl

import (
	"fmt"
	"os"
	"strings"

	"claude-go/internal/session"
	"golang.org/x/term"
)

// PickSession shows the interactive session resume list (like `claude -r`).
// Returns the chosen session id, or "" to start fresh / quit. When the
// all-projects view (ctrl-b) is active, entries outside cwd's project return
// the absolute .jsonl path instead — callers can detect the ".jsonl" suffix
// and use session.LoadPath.
//
// Keys: ↑/↓/j/k navigate · enter resume · / or ctrl-f search · ctrl-r rename
// · space preview pane · ctrl-b all projects · esc/ctrl-c cancel.
func PickSession(cwd string) string {
	infos, err := session.List(cwd)
	if err != nil {
		infos = nil
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

	const maxRows = 15
	const maxPreview = 20
	curSlug := session.Slug(cwd)

	var (
		sel        int
		searchMode bool
		query      []rune
		renameMode bool
		rbuf       []rune
		preview    bool
		allProj    bool
		view       []session.Info
		pcache     = map[string][]string{}
		lastLines  int
	)

	refilter := func() {
		view = view[:0]
		q := strings.ToLower(string(query))
		for _, in := range infos {
			if q == "" {
				view = append(view, in)
				continue
			}
			hay := strings.ToLower(in.Title + " " + in.Preview + " " + in.ID + " " + in.Slug)
			if subseq(q, hay) {
				view = append(view, in)
			}
		}
		if sel >= len(view) {
			sel = len(view) - 1
		}
		if sel < 0 {
			sel = 0
		}
	}

	reload := func() {
		var err error
		if allProj {
			infos, err = session.ListAll()
		} else {
			infos, err = session.List(cwd)
		}
		if err != nil {
			infos = nil
		}
		sel = 0
		pcache = map[string][]string{}
		refilter()
	}
	refilter()

	pick := func(in session.Info) string {
		if in.Slug == curSlug {
			return in.ID
		}
		return in.Path
	}

	draw := func() {
		if lastLines > 0 {
			fmt.Printf("\x1b[%dA", lastLines)
		}
		fmt.Print("\r\x1b[J")
		lastLines = 0
		emit := func(s string) {
			if lastLines > 0 {
				fmt.Print("\r\n")
			}
			fmt.Print(s)
			lastLines++
		}
		scope := "this project"
		if allProj {
			scope = "all projects"
		}
		emit(fmt.Sprintf("%sResume session — %s%s  %s(↑/↓ select · enter resume · / search · ^R rename · space preview · ^B scope · esc cancel)%s",
			bold, scope, reset, dim, reset))
		switch {
		case renameMode:
			emit(fmt.Sprintf("  %srename:%s %s█", cyan, reset, string(rbuf)))
		case searchMode || len(query) > 0:
			emit(fmt.Sprintf("  %sfilter:%s %s", cyan, reset, string(query)))
		}
		if len(view) == 0 {
			emit(fmt.Sprintf("  %s(no sessions)%s", dim, reset))
		}
		max := len(view)
		if max > maxRows {
			max = maxRows
		}
		for i := 0; i < max; i++ {
			in := view[i]
			pv := in.Label()
			if len(pv) > 60 {
				pv = pv[:60] + "…"
			}
			line := fmt.Sprintf("  %s  %s", in.Modified.Format("2006-01-02 15:04"), pv)
			if allProj {
				sp := session.SlugPath(in.Slug)
				if len(sp) > 40 {
					sp = "…" + sp[len(sp)-40:]
				}
				line += fmt.Sprintf("  %s%s%s", dim, sp, reset)
			}
			if i == sel {
				emit(fmt.Sprintf("%s❯%s%s", cyan, reset, line))
			} else {
				emit(fmt.Sprintf(" %s%s%s", dim, line, reset))
			}
		}
		if preview && len(view) > 0 && sel < len(view) {
			p := view[sel].Path
			block, ok := pcache[p]
			if !ok {
				block = session.PreviewBlock(p, maxPreview)
				pcache[p] = block
			}
			if len(block) > 0 {
				emit(fmt.Sprintf("  %s─── preview ───%s", dim, reset))
				for _, l := range block {
					emit(fmt.Sprintf("  %s%s%s", dim, l, reset))
				}
			}
		}
	}
	draw()

	for k := range ks.ch {
		// navigation works in every mode
		nav := func() bool {
			switch {
			case k.seq == "[A" || (!searchMode && !renameMode && k.b == 'k'):
				if sel > 0 {
					sel--
				}
			case k.seq == "[B" || (!searchMode && !renameMode && k.b == 'j'):
				if sel < len(view)-1 {
					sel++
				}
			default:
				return false
			}
			return true
		}
		if nav() {
			draw()
			continue
		}
		if renameMode {
			switch {
			case (k.b == 0x1b && k.seq == "") || k.b == 3:
				renameMode = false
			case k.b == '\r' || k.b == '\n':
				t := strings.TrimSpace(string(rbuf))
				if t != "" && len(view) > 0 {
					in := view[sel]
					if session.WriteTitle(in.Dir, in.ID, t) == nil {
						for i := range infos {
							if infos[i].Path == in.Path {
								infos[i].Title = t
							}
						}
						refilter()
					}
				}
				renameMode = false
			case k.b == 0x7f || k.b == 8:
				if len(rbuf) > 0 {
					rbuf = rbuf[:len(rbuf)-1]
				}
			case k.paste != "":
				rbuf = append(rbuf, []rune(k.paste)...)
			case k.r != 0:
				rbuf = append(rbuf, k.r)
			case k.b >= 32 && k.b < 127:
				rbuf = append(rbuf, rune(k.b))
			}
			draw()
			continue
		}
		if searchMode {
			switch {
			case (k.b == 0x1b && k.seq == "") || k.b == 3:
				searchMode = false
				query = query[:0]
				refilter()
			case k.b == '\r' || k.b == '\n':
				if len(view) > 0 {
					return pick(view[sel])
				}
				searchMode = false
			case k.b == 0x7f || k.b == 8:
				if len(query) > 0 {
					query = query[:len(query)-1]
				} else {
					searchMode = false
				}
				refilter()
			case k.paste != "":
				query = append(query, []rune(k.paste)...)
				refilter()
			case k.r != 0:
				query = append(query, k.r)
				refilter()
			case k.b >= 32 && k.b < 127:
				query = append(query, rune(k.b))
				refilter()
			}
			draw()
			continue
		}
		switch {
		case k.b == '\r' || k.b == '\n':
			if len(view) > 0 {
				return pick(view[sel])
			}
		case (k.b == 0x1b && k.seq == "") || k.b == 3:
			fmt.Print("\r\n")
			return ""
		case k.b == '/' || k.b == 6: // '/' or ctrl-f -> search
			searchMode = true
		case k.b == 18: // ctrl-r -> rename
			if len(view) > 0 {
				renameMode = true
				rbuf = []rune(view[sel].Title)
			}
		case k.b == ' ': // space -> preview pane
			preview = !preview
		case k.b == 2: // ctrl-b -> all projects toggle
			allProj = !allProj
			reload()
		}
		draw()
	}
	return ""
}
