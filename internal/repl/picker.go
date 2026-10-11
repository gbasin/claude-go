package repl

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gbasin/claude-go/internal/bg"
	"github.com/gbasin/claude-go/internal/session"
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
	return pickSession(cwd, nil, "")
}

// pickSession runs the picker against an existing keySource when the
// caller is the REPL — a second stdin reader would race the REPL's and
// starve the picker of arrows/enter (live-verified on m5p). nil starts
// a standalone reader for the non-REPL -r path.
func pickSession(cwd string, ks *keySource, selfID string) string {
	infos, err := session.List(cwd)
	if err != nil {
		infos = nil
	}
	if selfID != "" {
		kept := infos[:0]
		for _, in := range infos {
			if in.ID != selfID { // you can't resume the session you're in
				kept = append(kept, in)
			}
		}
		infos = kept
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		session.Print(cwd)
		return ""
	}
	if ks == nil {
		if ks, err = startKeys(); err != nil {
			session.Print(cwd)
			return ""
		}
		defer ks.stop()
	}

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
		curBranch  bool // ctrl-b: sessions on the current git branch only
		view       []session.Info
		pcache     = map[string][]string{}
		lastLines  int
		notice     string // transient hint under the list
	)

	// bg sessions run under a bg-host — real marks them `bg` in the list and
	// refuses a live-resume ("attach to it from claude agents or stop it")
	running := map[string]string{}
	for _, j := range bg.List() {
		running[j.SessionID] = j.Short
	}

	// real's branch filter matches the session's recorded git branch
	myBranch := session.CurrentGitBranch(cwd)
	refilter := func() {
		view = view[:0]
		q := strings.ToLower(string(query))
		for _, in := range infos {
			if curBranch && in.Branch != "" && in.Branch != myBranch {
				continue
			}
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
	// tryPick blocks resuming a still-running bg session — real refuses
	// ("attach to it from claude agents or stop it first"). The caller's
	// draw() renders the notice.
	tryPick := func() (string, bool) {
		in := view[sel]
		if short, ok := running[in.ID]; ok {
			notice = fmt.Sprintf("%s is still running — `claude attach %s` to join it", short, short)
			return "", false
		}
		return pick(in), true
	}

	width, height, _ := term.GetSize(int(os.Stdout.Fd()))
	if width <= 0 {
		width = 80
	}
	gray244 := "\x1b[38;5;244m"

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
			lastLines += 1 + visWidth(s)/width
		}
		// real 2.1.292: "Resume session (i of N)" header, bordered
		// ⌕ Search… box, two-row entries (title + "ago · branch · size"),
		// and the multi-key footer hints
		emit(fmt.Sprintf("%sResume session (%d of %d)%s", bold, min(sel+1, len(view)), len(view), reset))
		q := string(query)
		box := " " + gray244 + "╭" + strings.Repeat("─", width-4) + "╮" + reset
		emit(box)
		searchLine := "  " + gray244 + "│" + reset + "⌕ Search…"
		if searchMode || q != "" {
			searchLine = "  " + gray244 + "│" + reset + "⌕ " + q
		}
		emit(searchLine)
		emit(" " + gray244 + "╰" + strings.Repeat("─", width-4) + "╯" + reset)
		if renameMode {
			emit(fmt.Sprintf("  %srename to:%s %s█", cyan, reset, string(rbuf)))
		}
		if len(view) == 0 {
			emit(fmt.Sprintf("  %s(no sessions)%s", dim, reset))
		}
		max := len(view)
		if max > maxRows {
			max = maxRows
		}
		if r := height - 10; r < max {
			max = r
		}
		if max < 1 {
			max = 1
		}
		for i := 0; i < max; i++ {
			in := view[i]
			title := strings.ReplaceAll(strings.ReplaceAll(in.Label(), "\r", " "), "\n", " ")
			if visWidth(title) > width-10 {
				title = trunc(title, width-10)
			}
			meta := fmt.Sprintf("%s · %s · %s", relAgo(in.Modified), inBranch(in), humanKB(in.Size))
			if allProj {
				meta += "  " + dim + tailRunes(session.SlugPath(in.Slug), 30) + reset
			}
			if short, ok := running[in.ID]; ok {
				meta += fmt.Sprintf("  %sbg:%s%s", yellow, short, reset)
			}
			if i == sel {
				emit(fmt.Sprintf(" %s❯%s %s", cyan, "\x1b[39m", title))
				emit("    " + dim + meta + reset)
			} else {
				emit("   " + title)
				emit("    " + dim + meta + reset)
			}
		}
		// real's footer: Ctrl+A toggles all-projects, Ctrl+B toggles the
		// current-branch filter (mod_1935 picker hints)
		aLabel := "show all projects"
		if allProj {
			aLabel = "only show current repo"
		}
		bLabel := "show all branches"
		if curBranch {
			bLabel = "only show current branch"
		}
		emit(" " + dim + "Ctrl+A to " + aLabel + " · Type to search · Esc to cancel" + reset)
		emit(" " + dim + "Ctrl+B to " + bLabel + " · Space to preview · Ctrl+R to rename" + reset)
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
					emit(fmt.Sprintf("  %s%s%s", dim, trunc(l, width-6), reset))
				}
			}
		}
		if notice != "" {
			emit(fmt.Sprintf("  %s%s%s", yellow, notice, reset))
		}
	}
	draw()

	for k := range ks.ch {
		notice = ""
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
					if id, ok := tryPick(); ok {
						return id
					}
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
		// Select-context remaps resolve first (real: pickers run the
		// same keymap); hardcoded cases stay as the fallback layer.
		// Global-context actions (ctrl+c interrupt) fall through.
		if act := dialogAction("Select", k); strings.HasPrefix(act, "select:") {
			switch act {
			case "select:previous":
				if sel > 0 {
					sel--
				}
			case "select:next":
				if sel < len(view)-1 {
					sel++
				}
			case "select:first":
				sel = 0
			case "select:last":
				sel = len(view) - 1
			case "select:pageUp":
				sel -= 10
				if sel < 0 {
					sel = 0
				}
			case "select:pageDown":
				sel += 10
				if sel > len(view)-1 {
					sel = len(view) - 1
				}
			case "select:accept":
				if len(view) > 0 {
					if id, ok := tryPick(); ok {
						return id
					}
				}
			case "select:cancel":
				fmt.Print("\r\n")
				return ""
			}
			draw()
			continue
		}
		switch {
		case k.b == '\r' || k.b == '\n':
			if len(view) > 0 {
				if id, ok := tryPick(); ok {
					return id
				}
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
		case k.b == 1: // ctrl-a -> all projects (real's binding)
			allProj = !allProj
			reload()
		case k.b == 2: // ctrl-b -> current-branch filter
			curBranch = !curBranch
			refilter()
		}
		draw()
	}
	return ""
}

// relAgo renders "N minutes/hours/days ago" like real's picker meta row.
func relAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

func inBranch(in session.Info) string {
	if in.Branch != "" {
		return in.Branch
	}
	return "HEAD"
}

func humanKB(sz int64) string {
	if sz < 1024 {
		return fmt.Sprintf("%dB", sz)
	}
	if sz < 1<<20 {
		return fmt.Sprintf("%.1fKB", float64(sz)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(sz)/(1<<20))
}
