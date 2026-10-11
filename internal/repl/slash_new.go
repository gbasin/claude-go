// slash_new.go — handlers added for the 2.1.294 slash-parity pass
// (audits/slash.md): previously-missing commands, real argument semantics,
// and the prompt-bar color command split out of /theme.

package repl

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gbasin/claude-go/internal/agent"
	"github.com/gbasin/claude-go/internal/api"
	"github.com/gbasin/claude-go/internal/bg"
	"github.com/gbasin/claude-go/internal/bridge"
	"github.com/gbasin/claude-go/internal/config"
	"github.com/gbasin/claude-go/internal/plugins"
	"github.com/gbasin/claude-go/internal/session"
	"github.com/gbasin/claude-go/internal/tools"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// ---------- /resume <id|search term> ----------

// resolveSessionArg maps real's `/resume [conversation id or search term]`
// argument to a session in this project: exact/prefix id match wins, then a
// substring match on title/preview/slug. "" means "open the picker".
func resolveSessionArg(cwd, selfID, arg string) string {
	infos, err := session.List(cwd)
	if err != nil {
		return ""
	}
	q := strings.ToLower(arg)
	var match []session.Info
	for _, in := range infos {
		if in.ID == selfID {
			continue
		}
		if in.ID == arg || strings.HasPrefix(in.ID, arg) {
			return in.ID
		}
		hay := strings.ToLower(in.Title + " " + in.Preview + " " + in.ID)
		if strings.Contains(hay, q) {
			match = append(match, in)
		}
	}
	if len(match) == 1 {
		return match[0].ID
	}
	return ""
}

// ---------- /code-review argument grammar ----------

// codeReviewLevels mirrors real's level list (ue/Dc): the same effort names
// /effort takes minus "auto" — ultra is a subcommand, not a level.
var codeReviewLevels = []string{"low", "medium", "high", "xhigh", "max"}

type codeReviewArgs struct {
	level        string // explicit effort level
	target       string // <pr#>|<branch>|<path>
	rest         string // re-joined args for the ultra dispatch
	ultra        bool
	fix, comment bool
	maxFindings  string // "<n>" | "all" | "default"
	unrecognized string // near-miss level word (real: "Ignoring unrecognized effort")
}

// parseCodeReviewArgs is mod_0683 Ce(): the first bare token is an effort
// level, "ultra", or part of the target; --fix/--comment/--post/--no-post
// are flags; --max-findings takes <n>|all|default.
func parseCodeReviewArgs(arg string) codeReviewArgs {
	var out codeReviewArgs
	var bare []string
	fields := strings.Fields(arg)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch {
		case f == "--fix":
			out.fix = true
		case f == "--comment":
			out.comment = true
		case f == "--post" || f == "--no-post":
			out.rest += " " + f
		case f == "--max-findings" && i+1 < len(fields):
			i++
			out.maxFindings = fields[i]
		case strings.HasPrefix(f, "--max-findings="):
			out.maxFindings = strings.TrimPrefix(f, "--max-findings=")
		case strings.HasPrefix(f, "--"):
			out.rest += " " + f
		default:
			bare = append(bare, f)
		}
	}
	if len(bare) > 0 {
		first := strings.ToLower(bare[0])
		switch {
		case first == "ultra":
			out.ultra = true
			out.rest = strings.TrimSpace(strings.Join(bare[1:], " ") + out.rest)
		case isReviewLevel(first):
			out.level = first
			out.target = strings.Join(bare[1:], " ")
		default:
			// real flags words sharing a level's 3-char prefix that aren't
			// one ("his", "medical"…) as an unrecognized level
			for _, lv := range codeReviewLevels {
				if len(first) >= 3 && first[:3] == lv[:3] && !isReviewLevel(first) {
					out.unrecognized = bare[0]
				}
			}
			out.target = strings.Join(bare, " ")
		}
	}
	if out.ultra {
		out.rest = strings.TrimSpace(out.rest)
	}
	return out
}

func isReviewLevel(s string) bool {
	for _, lv := range codeReviewLevels {
		if s == lv {
			return true
		}
	}
	return false
}

// ---------- /color — prompt bar color for this session ----------

// promptBarColors is real's named prompt-bar palette (y_ + "default" in
// mod_0416). Session-scoped — nothing persists.
var promptBarColors = []struct{ name, ansi string }{
	{"red", "\x1b[31m"}, {"orange", "\x1b[38;5;208m"}, {"yellow", "\x1b[33m"},
	{"green", "\x1b[32m"}, {"cyan", "\x1b[36m"}, {"blue", "\x1b[34m"},
	{"purple", "\x1b[35m"}, {"pink", "\x1b[38;5;213m"},
}

func colorCmd(ed *editor, arg string) {
	if arg == "" {
		var names []string
		for _, c := range promptBarColors {
			names = append(names, c.name)
		}
		dprintf("  prompt bar color — /color <%s|default>\n", strings.Join(names, "|"))
		return
	}
	if arg == "default" {
		ed.prompt = " ❯\u00a0"
		dprintln("  prompt bar color reset")
		return
	}
	for _, c := range promptBarColors {
		if arg == c.name {
			ed.prompt = " " + c.ansi + "❯\u00a0" + reset
			// real's saveAgentColor (XKt): the session color latches into
			// the transcript as {type:"agent-color"} and re-stamps per flush
			if currentStore != nil {
				currentStore.LogAgentMeta("", "", c.name, "")
			}
			dprintf("  prompt bar color → %s%s%s\n", c.ansi, c.name, reset)
			return
		}
	}
	var names []string
	for _, c := range promptBarColors {
		names = append(names, c.name)
	}
	dprintf("  unknown color %q — options: %s, default\n", arg, strings.Join(names, ", "))
}

// ---------- /mobile, /session — QR surfaces ----------

func mobileCmd() {
	// real (mod_0416 Bco): QR code to download the Claude mobile app
	dprintln("  " + bold + "Get the Claude mobile app" + reset)
	for _, r := range qrText("https://claude.ai/mobile") {
		dprintln(r)
	}
	dprintln("  " + dim + "scan, or open https://claude.ai/mobile" + reset)
}

func sessionCmd(l *agent.Loop) {
	// real (mod_0416 qco): "Show cloud session URL and QR code", alias
	// /remote — needs the remote-control bridge session
	id := tools.BridgeSessionID()
	if id == "" {
		dprintln("  this session isn't on claude.ai/code — run /remote-control on to connect it")
		return
	}
	url := bridge.SessionWebURL(id, "")
	dprintln("  " + url)
	for _, r := range qrText(url) {
		dprintln(r)
	}
}

// ---------- /desktop ----------

func desktopCmd(l *agent.Loop) {
	// real (mod_0416 eco, alias /app): "Continue the current session in
	// Claude Desktop" — hands the session to the desktop app via its
	// claude:// deep link when the app is installed
	if id := tools.BridgeSessionID(); id != "" {
		url := bridge.SessionWebURL(id, "")
		dprintln("  Continue this session in Claude Desktop:", url)
		if !openURL(url) {
			dprintln("  (copy the link into Claude Desktop)")
		}
		return
	}
	dprintln("  Claude Desktop continues sessions started here once the session is connected —")
	dprintln("  run /remote-control on, or get the app at https://claude.com/download")
}

// ---------- /list-agents ----------

// sessRegEntry mirrors tools' session registry rows
// (~/.claude/sessions/<pid>.json) — read-only projection.
type sessRegEntry struct {
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Name      string `json:"name"`
	PID       int    `json:"pid"`
	Kind      string `json:"kind"`
}

func listAgentsCmd(l *agent.Loop) {
	// real (mod_1206): "List subagents, teammates, and other Claude
	// sessions you can message"
	var b strings.Builder
	for _, a := range tools.ListInProcessAgents() {
		st := "running"
		if a.Completed() {
			st = "completed"
		}
		fmt.Fprintf(&b, "  %s [%s]  in-process  %s  type=%s\n",
			a.Name, shortAgentID(a.AgentID), st, a.Type)
	}
	for _, e := range liveSessionEntries() {
		label := "local-session"
		if l.Client.SessionID == e.SessionID {
			label = "local-session (this session)"
		}
		fmt.Fprintf(&b, "  %s [%s]  %s  cwd=%s pid=%d\n",
			e.Name, shortAgentID(e.SessionID), label, e.CWD, e.PID)
	}
	if l.ToolCtx.BgTasks != nil {
		tasks := l.ToolCtx.BgTasks.List()
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
		for _, t := range tasks {
			st := "running"
			select {
			case <-t.Done:
				st = "exited"
			default:
			}
			fmt.Fprintf(&b, "  %s  background-task  [%s] %s\n", t.ID, st, t.Command)
		}
	}
	if b.Len() == 0 {
		dprintln("  no agents or sessions found")
		return
	}
	dprintf("  %sAgents you can message%s\n%s", bold, reset, b.String())
}

func shortAgentID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func liveSessionEntries() []sessRegEntry {
	var out []sessRegEntry
	for _, p := range []string{
		filepath.Join(config.ClaudeDir(), "sessions"),
		filepath.Join(config.Home(), ".claude", "sessions"),
	} {
		ents, err := filepath.Glob(filepath.Join(p, "*.json"))
		if err != nil {
			continue
		}
		for _, f := range ents {
			var e sessRegEntry
			if b, err := os.ReadFile(f); err == nil &&
				json.Unmarshal(b, &e) == nil && e.SessionID != "" && procAlive(e.PID) {
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// ---------- /recap ----------

// recapCmd is real's /recap (mod_1210): a one-line session recap via an
// auxiliary model call over the conversation tail.
func recapCmd(l *agent.Loop) {
	if len(l.Messages) == 0 {
		dprintln("  Nothing to recap yet — send a message first.")
		return
	}
	dprintln("  generating recap…")
	go func() {
		var digest strings.Builder
		for _, m := range l.Messages {
			var txt string
			var s string
			if json.Unmarshal(m.Content, &s) == nil {
				txt = s
			} else {
				var blocks []api.ContentBlock
				if json.Unmarshal(m.Content, &blocks) == nil {
					for _, b := range blocks {
						if b.Type == "text" {
							txt += b.Text
						}
					}
				}
			}
			txt = strings.TrimSpace(txt)
			if txt == "" {
				continue
			}
			if len(txt) > 800 {
				txt = txt[:800] + "…"
			}
			fmt.Fprintf(&digest, "%s: %s\n", m.Role, txt)
			if digest.Len() > 48000 {
				break // tail is what matters for a recap
			}
		}
		// one-shot aux call on a fresh loop — same field set /btw uses
		sub := &agent.Loop{
			Client: l.Client, Model: "claude-haiku-4-5-20251001",
			MaxTokens: l.MaxTokens, Entrypoint: l.Entrypoint,
			PermMode: l.PermMode, CWD: l.CWD, AddDirs: l.AddDirs,
			User: l.User, ToolCtx: l.ToolCtx, Events: l.Events,
			Transcript: l.Transcript, Hooks: l.Hooks, Bare: true,
		}
		resp, err := sub.Ask(context.Background(),
			"Write a one-line recap of this session — what the user wanted and where things stand. Plain text, no preamble.\n\n"+digest.String())
		if err != nil {
			dprintln("\n  Couldn't generate a recap. Run with --debug for details.")
			return
		}
		var txt string
		for _, b := range resp.Content {
			if b.Type == "text" {
				txt += b.Text
			}
		}
		if txt == "" {
			dprintln("\n  Couldn't generate a recap. Run with --debug for details.")
			return
		}
		dprintf("\n  %s⎿%s %s\n", dim, reset, strings.TrimSpace(txt))
	}()
}

// ---------- /tui ----------

// tuiCmd is /tui [default|fullscreen] — the terminal renderer setting.
// Ours has a single renderer; the choice persists like real's setting so a
// later fullscreen renderer can pick it up.
func tuiCmd(arg string) {
	cur, _ := config.UserSetting("tui").(string)
	switch arg {
	case "":
		if cur == "" {
			cur = "default"
		}
		dprintf("  terminal UI renderer: %s — /tui <default|fullscreen> to change\n", cur)
	case "default", "fullscreen":
		config.WriteUserSetting("tui", arg)
		dprintf("  terminal UI renderer → %s\n", arg)
		if arg == "fullscreen" {
			dprintln("  " + dim + "fullscreen layout isn't in this build — the setting is saved" + reset)
		}
	default:
		dprintln("  usage: /tui <default|fullscreen>")
	}
}

// ---------- /install ----------

func installCmd(arg string) {
	// real (mod_2048): "Install Claude Code native build" — our binary is
	// already a standalone native build; report where it lives
	exe, err := os.Executable()
	if err != nil {
		exe = "unknown"
	}
	dprintf("  claude-go %s is a native build at %s\n", api.Version, exe)
	dprintln("  " + dim + "nothing to install — update by rebuilding or reinstalling the binary" + reset)
}

// ---------- /workflows ----------

func workflowsCmd(l *agent.Loop) {
	// real (mod_1204): "Browse running and completed workflows" — runs land
	// in BgTasks as "workflow <runID>" plus per-run dirs with a journal
	type run struct{ id, status string }
	var runs []run
	seen := map[string]bool{}
	if l.ToolCtx.BgTasks != nil {
		for _, t := range l.ToolCtx.BgTasks.List() {
			if !strings.HasPrefix(t.Command, "workflow ") {
				continue
			}
			id := strings.TrimPrefix(t.Command, "workflow ")
			st := "running"
			select {
			case <-t.Done:
				st = "completed"
			default:
			}
			runs = append(runs, run{id, st})
			seen[id] = true
		}
	}
	dirs1, _ := filepath.Glob(filepath.Join(os.TempDir(), "claude-go-workflows", l.ToolCtx.Session, "wf_*"))
	dirs2, _ := filepath.Glob(filepath.Join(l.ToolCtx.CWD, ".claude", "workflows", "runs", "wf_*"))
	for _, d := range append(dirs1, dirs2...) {
		id := filepath.Base(d)
		if !seen[id] {
			st := "completed"
			if _, err := os.Stat(filepath.Join(d, "result.json")); err != nil {
				st = "finished"
			}
			runs = append(runs, run{id, st})
		}
	}
	if len(runs) == 0 {
		dprintln("  no workflows this session — the Workflow tool launches them in the background")
		return
	}
	dprintf("  %sWorkflows%s\n", bold, reset)
	for _, r := range runs {
		dprintf("    %-24s %s\n", r.id, r.status)
	}
}

// ---------- /daemon ----------

// daemonCmd is /daemon (mod_1209): "Manage background services and
// routines" — the bg-pty-host daemon status, live bg jobs, and this
// project's scheduled tasks.
func daemonCmd(l *agent.Loop, arg string) {
	switch arg {
	case "stop":
		if err := bg.StopDaemon(); err != nil {
			dprintln("  daemon stop failed:", err)
		} else {
			dprintln("  background daemon stopped")
		}
		return
	case "start":
		if _, err := bg.StartDaemon(); err != nil {
			dprintln("  daemon start failed:", err)
		} else {
			dprintf("  background daemon running (pid %d)\n", bg.DaemonPID())
		}
		return
	case "":
	default:
		dprintln("  usage: /daemon [start|stop]")
		return
	}
	dprintf("  %sDaemon%s\n", bold, reset)
	if bg.DaemonAlive() {
		dprintf("    bg host daemon: running (pid %d)\n", bg.DaemonPID())
	} else {
		dprintln("    bg host daemon: not running — starts on first /background")
	}
	jobs := bg.List()
	if len(jobs) == 0 {
		dprintln("    background sessions: none")
	} else {
		dprintln("    background sessions:")
		for _, j := range jobs {
			dprintf("      %s  %s  %s\n", j.Short, j.CWD, relAgo(j.StartedAt))
		}
	}
	var env struct {
		Tasks []struct {
			ID     string `json:"id"`
			Cron   string `json:"cron"`
			Prompt string `json:"prompt"`
		} `json:"tasks"`
	}
	if b, err := os.ReadFile(filepath.Join(l.ToolCtx.CWD, ".claude", "scheduled_tasks.json")); err == nil &&
		json.Unmarshal(b, &env) == nil && len(env.Tasks) > 0 {
		dprintln("    scheduled tasks:")
		for _, t := range env.Tasks {
			p := t.Prompt
			if len(p) > 50 {
				p = p[:50] + "…"
			}
			dprintf("      %s  %s  %s\n", t.ID, t.Cron, p)
		}
	}
}

// ---------- /plugin ----------

// pluginCmd is /plugin (mod_0416 Jbo, aliases plugins/marketplace): real
// opens the plugin manager; the text surface lists installed plugins,
// their state, and known marketplaces.
func pluginCmd(l *agent.Loop, invoked, arg string) {
	inst := plugins.List()
	mkts := plugins.ListMarketplaces()
	if invoked == "marketplace" || cmd0(arg) == "marketplace" {
		if len(mkts) == 0 {
			dprintln("  no marketplaces configured")
		}
		var names []string
		for n := range mkts {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			dprintf("    %s  %s%s%s\n", n, dim, mkts[n], reset)
		}
		return
	}
	dprintf("  %sPlugins%s\n", bold, reset)
	if len(inst) == 0 {
		dprintln("    " + dim + "(none installed — /plugin install <name>@<marketplace>)" + reset)
	}
	for _, p := range inst {
		st := "enabled"
		if !p.Enabled {
			st = "disabled"
		}
		dprintf("    %s%s%s@%s  %s  %s%s%s\n", bold, p.Name, reset, p.Marketplace, st, dim, p.Version, reset)
	}
	if len(mkts) > 0 {
		dprintf("  %sMarketplaces%s\n", bold, reset)
		var names []string
		for n := range mkts {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			dprintf("    %s  %s%s%s\n", n, dim, mkts[n], reset)
		}
	}
	dprintln("  " + dim + "/plugin install <name>@<marketplace> · /plugin marketplace add <src> · /reload-plugins" + reset)
}

func cmd0(arg string) string {
	f := strings.Fields(arg)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}
