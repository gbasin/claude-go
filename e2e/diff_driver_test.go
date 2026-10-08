//go:build e2e

// diff_driver_test.go — differential harness core: run REAL claude and
// claude-go through the same scenario against the same mock API
// implementation, record every POST /v1/messages body each emits, and
// diff the request streams structurally.
//
// Both sides are launched as external processes with ANTHROPIC_BASE_URL
// pointed at a per-run in-process mock.Server (internal/mock), so the
// recorded wire shape is directly comparable. Per-run values (uuids,
// timestamps, tmp/home paths) are normalized before diffing; remaining
// diffs are fidelity holes — they are REPORTED, not failed on.

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"claude-go/internal/mock"
)

// diffScenario is one identical input script driven through both binaries.
type diffScenario struct {
	name    string
	args    []string // CLI args, identical for both binaries
	timeout time.Duration

	// interactive scenarios run on a PTY instead of -p
	interactive bool
	// oauthOnly drops ANTHROPIC_API_KEY from the child env — real's REPL
	// gates on a "custom API key" approval dialog when it's set
	oauthOnly bool
	send        string // keystrokes once the prompt appears
	waitFor     string // substring marking the turn complete
	exitKeys    string // how to leave the REPL after the turn (default: ctrl-c x2)
}

// diffRun is one binary's recorded wire activity for a scenario.
type diffRun struct {
	label    string
	bin      string
	hits     []mock.Hit
	bodies   [][]byte       // raw POST /v1/messages bodies, in order
	headers  []http.Header // headers per body, same index
	stdout   string
	stderr   string
	code     int
	timedOut bool
	home     string
	cwd      string
}

// realClaudeBin locates the real claude binary; the subtest skips when
// it can't be found (e.g. CI without a local install).
func realClaudeBin(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("CLAUDE_REAL_BIN"); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".local", "bin", "claude")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	t.Skip("real claude binary not found (set CLAUDE_REAL_BIN)")
	return ""
}

// newDiffSandbox makes a fresh (cwd, home) pair seeded so neither binary
// blocks on onboarding/trust prompts.
func newDiffSandbox(t *testing.T) (cwd, home string) {
	t.Helper()
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval cwd: %v", err)
	}
	home, err = filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval home: %v", err)
	}
	claudeJSON := map[string]any{
		"hasCompletedOnboarding": true,
		"projects": map[string]any{
			cwd: map[string]any{"hasTrustDialogAccepted": true},
		},
	}
	b, _ := json.Marshal(claudeJSON)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), b, 0o644); err != nil {
		t.Fatalf("seed .claude.json: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir ~/.claude: %v", err)
	}
	return cwd, home
}

// diffEnv builds the identical child env both binaries get: isolated
// HOME, stubbed OS integrations, dummy credentials, mock base URL.
func diffEnv(home, mockAddr string, oauthOnly bool) []string {
	tmp := filepath.Join(home, ".tmp")
	os.MkdirAll(tmp, 0o755)
	env := []string{
		"HOME=" + home,
		"PATH=" + stubBinDir() + ":" + os.Getenv("PATH"),
		"SHELL=/bin/bash",
		"TERM=xterm-256color",
		"LANG=en_US.UTF-8",
		"TMPDIR=" + tmp,
		"USER=" + os.Getenv("USER"),
		"ANTHROPIC_BASE_URL=" + mockAddr,
		"ANTHROPIC_AUTH_TOKEN=test",
		"CLAUDE_CODE_OAUTH_TOKEN=test",
		"CLAUDE_GO_NO_BROWSER=1",
		"DISABLE_TELEMETRY=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
	if !oauthOnly {
		env = append(env, "ANTHROPIC_API_KEY=test")
	}
	return env
}

// execAgainstMock runs bin once against a fresh mock server and collects
// the recorded traffic.
func execAgainstMock(t *testing.T, label, bin string, sc diffScenario, cwd, home string) diffRun {
	t.Helper()
	srv, err := mock.Start()
	if err != nil {
		t.Fatalf("mock start: %v", err)
	}
	defer srv.Close()
	r := diffRun{label: label, bin: bin, home: home, cwd: cwd}

	timeout := sc.timeout
	if timeout == 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, sc.args...)
	cmd.Dir = cwd
	cmd.Env = diffEnv(home, srv.Addr, sc.oauthOnly)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err = cmd.Run()
	r.stdout, r.stderr = so.String(), se.String()
	r.timedOut = ctx.Err() == context.DeadlineExceeded
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			r.code = ee.ExitCode()
		} else {
			r.code = -1
		}
	}
	r.hits = append(r.hits, srv.Hits...)
	r.bodies = append(r.bodies, srv.Bodies...)
	r.headers = append(r.headers, srv.Headers...)
	if dir := os.Getenv("CLAUDE_GO_DIFF_DUMP"); dir != "" {
		os.MkdirAll(dir, 0o755)
		for i, b := range r.bodies {
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-body%d.json", label, i)), b, 0o644)
			var hdrs http.Header
			if i < len(srv.Headers) {
				hdrs = srv.Headers[i]
			}
			dumpGolden(t, dir, label, i, "POST /v1/messages?beta=true HTTP/1.1", hdrs, b)
		}
	}
	return r
}

// ptyAgainstMock runs bin on a PTY against a fresh mock, performs the
// single-turn script, and collects the recorded traffic.
func ptyAgainstMock(t *testing.T, label, bin string, sc diffScenario, cwd, home string) diffRun {
	t.Helper()
	srv, err := mock.Start()
	if err != nil {
		t.Fatalf("mock start: %v", err)
	}
	defer srv.Close()
	r := diffRun{label: label, bin: bin, home: home, cwd: cwd}

	master, slave := openPTYSize(t, 120, 40)
	cmd := exec.Command(bin, sc.args...)
	cmd.Dir = cwd
	cmd.Env = diffEnv(home, srv.Addr, sc.oauthOnly)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = ptyProcAttr()
	if err := cmd.Start(); err != nil {
		master.Close()
		slave.Close()
		t.Fatalf("%s: start on pty: %v", label, err)
	}
	slave.Close()
	p := &ptyProc{t: t, cmd: cmd, master: master, done: make(chan error, 1)}
	go func() {
		var tmp [4096]byte
		for {
			n, err := master.Read(tmp[:])
			if n > 0 {
				p.mu.Lock()
				p.buf.Write(tmp[:n])
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { p.done <- cmd.Wait() }()
	defer p.close()

	// wait for the prompt, send one turn, wait for the mock's reply
	p.waitFor("❯", 45*time.Second)
	p.send(sc.send)
	p.waitFor(sc.waitFor, 60*time.Second)

	exit := sc.exitKeys
	if exit == "" {
		exit = "/exit\r"
	}
	p.sendQuiet(exit)
	exited := false
	select {
	case err := <-p.done:
		p.done <- err // preserve for p.close()'s wait
		exited = true
	case <-time.After(5 * time.Second):
	}
	if !exited {
		p.sendQuiet("\x03\x03") // ctrl-c fallback
		select {
		case err := <-p.done:
			p.done <- err
		case <-time.After(3 * time.Second):
		}
	}
	r.stdout = p.clean()
	r.hits = append(r.hits, srv.Hits...)
	r.bodies = append(r.bodies, srv.Bodies...)
	r.headers = append(r.headers, srv.Headers...)
	if dir := os.Getenv("CLAUDE_GO_DIFF_DUMP"); dir != "" {
		os.MkdirAll(dir, 0o755)
		for i, b := range r.bodies {
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-body%d.json", label, i)), b, 0o644)
			var hdrs http.Header
			if i < len(srv.Headers) {
				hdrs = srv.Headers[i]
			}
			dumpGolden(t, dir, label, i, "POST /v1/messages?beta=true HTTP/1.1", hdrs, b)
		}
	}
	return r
}

// volatileHeaders are values that legitimately differ between the two
// binaries (tokens, ids, sizes) — presence/choice is compared, not bytes.
var volatileHeaders = map[string]bool{
	"authorization": true, "x-api-key": true, "content-length": true,
	"x-claude-code-session-id": true, "x-client-request-id": true,
	"x-stainless-retry-count": true, "x-claude-code-prompt-id": true,
	"host": true, "user-agent": false, // version must match — compared
}

// absorbConditional drops fields whose presence varies with real's
// per-session GB/auto-mode rollout — the shape still compares when
// both sides emit them.
func absorbConditional(a, b any) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if !aok || !bok {
		return
	}
	_, ra := am["safeguards"]
	_, rb := bm["safeguards"]
	if ra != rb {
		delete(am, "safeguards")
		delete(bm, "safeguards")
	}
}

// conditionalBetas ride real's per-session auto-mode / GB rollout
// decisions — their presence in the header set varies run to run.
var conditionalBetas = map[string]bool{
	"dangerous-tool-use-2026-09-03": true,
	"afk-mode-2026-01-31":          true,
}

func stripConditionalBetas(v string) string {
	parts := strings.Split(v, ",")
	out := parts[:0]
	for _, p := range parts {
		if !conditionalBetas[p] {
			out = append(out, p)
		}
	}
	return strings.Join(out, ",")
}

func diffHeaders(d *differ, a, b http.Header) {
	am, bm := map[string]string{}, map[string]string{}
	for k, v := range a {
		am[strings.ToLower(k)] = strings.Join(v, ",")
	}
	for k, v := range b {
		bm[strings.ToLower(k)] = strings.Join(v, ",")
	}
	for k, av := range am {
		bv, ok := bm[k]
		if !ok {
			d.out = append(d.out, fmt.Sprintf("  headers[%q]: real=%q ours=<missing>", k, truncStr(av, 80)))
			continue
		}
		if volatileHeaders[k] {
			continue
		}
		if k == "anthropic-beta" {
			av = stripConditionalBetas(av)
			bv = stripConditionalBetas(bv)
		}
		if av != bv {
			d.out = append(d.out, fmt.Sprintf("  headers[%q]: real=%q ours=%q", k,
				truncStr(av, 110), truncStr(bv, 110)))
		}
	}
	for k, bv := range bm {
		if _, ok := am[k]; !ok {
			d.out = append(d.out, fmt.Sprintf("  headers[%q]: real=<missing> ours=%q", k, truncStr(bv, 80)))
		}
	}
}

// dumpGolden writes a wireCapture-shaped golden (request_line + headers +
// body) for regenerating testdata goldens from live runs.
func dumpGolden(t *testing.T, dir, label string, i int, reqLine string, hdrs http.Header, body []byte) {
	t.Helper()
	out := map[string]any{
		"request_line": reqLine,
		"headers":      map[string]any{},
	}
	for k, vs := range hdrs {
		lk := strings.ToLower(k)
		for _, v := range vs {
			out["headers"].(map[string]any)[lk] = v
		}
	}
	var bodyObj any
	json.Unmarshal(body, &bodyObj)
	out["body"] = bodyObj
	raw, _ := json.MarshalIndent(out, "", "  ")
	os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-golden%d.json", label, i)), raw, 0o644)
}

// --- normalization -----------------------------------------------------------

var (
	uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	tsRe   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})`)
	tmpRe  = regexp.MustCompile(`/(private/)?(tmp|var/folders)/[^\s"'\\]*`)
	msgidRe = regexp.MustCompile(`msg_[A-Za-z0-9_]+`)
	// device_id lives inside the JSON-encoded metadata.user_id string —
	// its quotes arrive escaped (\"...\")
	devidRe = regexp.MustCompile(`\\"[a-f0-9]{64}\\"`)
)

// normalizeWire scrubs per-run values out of a raw request body so the
// structural diff only reports real divergence.
func normalizeWire(raw []byte, r diffRun) []byte {
	s := string(raw)
	s = strings.ReplaceAll(s, r.home, "<HOME>")
	s = strings.ReplaceAll(s, r.cwd, "<CWD>")
	s = tmpRe.ReplaceAllString(s, "<TMP>")
	s = uuidRe.ReplaceAllString(s, "<uuid>")
	s = tsRe.ReplaceAllString(s, "<ts>")
	s = msgidRe.ReplaceAllString(s, "<msgid>")
	// device_id is a per-install random 64-hex (real's userID in
	// .claude.json) — normalize like the other per-install ids
	s = devidRe.ReplaceAllString(s, "<devid>")
	return []byte(s)
}

// --- structural comparator ----------------------------------------------------

const maxFieldDiffs = 120

type differ struct{ out []string }

func (d *differ) add(path, format string, args ...any) {
	if len(d.out) >= maxFieldDiffs {
		return
	}
	d.out = append(d.out, fmt.Sprintf("  %s: %s", path, fmt.Sprintf(format, args...)))
}

func short(v any) string {
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// namedList reports whether every element is an object with a string
// "name" key — tools[], mcp servers etc. compare keyed-by-name instead
// of by index so insertions don't cascade.
func namedList(a []any) bool {
	if len(a) == 0 {
		return false
	}
	for _, e := range a {
		m, ok := e.(map[string]any)
		if !ok {
			return false
		}
		if _, ok := m["name"].(string); !ok {
			return false
		}
	}
	return true
}

func (d *differ) value(path string, a, b any) {
	if len(d.out) >= maxFieldDiffs {
		return
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			d.add(path, "real=object ours=%s", short(b))
			return
		}
		keys := map[string]bool{}
		for k := range av {
			keys[k] = true
		}
		for k := range bv {
			keys[k] = true
		}
		var sorted []string
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			ea, oka := av[k]
			eb, okb := bv[k]
			child := path + "." + k
			switch {
			case !oka:
				d.add(child, "real=<missing> ours=%s", short(eb))
			case !okb:
				d.add(child, "real=%s ours=<missing>", short(ea))
			default:
				d.value(child, ea, eb)
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			d.add(path, "real=array[%d] ours=%s", len(av), short(b))
			return
		}
		if namedList(av) && namedList(bv) {
			d.byName(path, av, bv)
			return
		}
		if len(av) != len(bv) {
			d.add(path, "length real=%d ours=%d", len(av), len(bv))
		}
		for i := 0; i < len(av) || i < len(bv); i++ {
			if len(d.out) >= maxFieldDiffs {
				return
			}
			child := fmt.Sprintf("%s[%d]", path, i)
			switch {
			case i >= len(bv):
				d.add(child, "real=%s ours=<missing>", short(av[i]))
			case i >= len(av):
				d.add(child, "real=<missing> ours=%s", short(bv[i]))
			default:
				d.value(child, av[i], bv[i])
			}
		}
	default:
		if fmt.Sprint(a) != fmt.Sprint(b) {
			d.add(path, "real=%s ours=%s", short(a), short(b))
		}
	}
}

// byName diffs two name-keyed object arrays: reports missing elements and
// recurses into pairs sharing a name.
func (d *differ) byName(path string, a, b []any) {
	index := func(l []any) map[string]any {
		m := map[string]any{}
		for _, e := range l {
			em := e.(map[string]any)
			n := em["name"].(string)
			if _, dup := m[n]; !dup {
				m[n] = e
			}
		}
		return m
	}
	am, bm := index(a), index(b)
	var names []string
	seen := map[string]bool{}
	for _, e := range a {
		n := e.(map[string]any)["name"].(string)
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, e := range b {
		n := e.(map[string]any)["name"].(string)
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, n := range names {
		ea, oka := am[n]
		eb, okb := bm[n]
		child := fmt.Sprintf("%s[%q]", path, n)
		switch {
		case !oka:
			d.add(child, "real=<missing> ours=present")
		case !okb:
			d.add(child, "real=present ours=<missing>")
		default:
			d.value(child, ea, eb)
		}
	}
}

// --- report --------------------------------------------------------------------

func hitsLine(hits []mock.Hit) string {
	var parts []string
	for _, h := range hits {
		parts = append(parts, fmt.Sprintf("%s %s->%d", h.Method, h.Path, h.Status))
	}
	return strings.Join(parts, "  ")
}

// runDiffScenario executes sc through both binaries and prints the
// field-level comparison table. It returns the number of diff lines.
func runDiffScenario(t *testing.T, sc diffScenario) {
	t.Helper()
	realBin := realClaudeBin(t)

	cwdR, homeR := newDiffSandbox(t)
	cwdO, homeO := newDiffSandbox(t)

	var real, ours diffRun
	if sc.interactive {
		real = ptyAgainstMock(t, "real", realBin, sc, cwdR, homeR)
		ours = ptyAgainstMock(t, "ours", binPath, sc, cwdO, homeO)
	} else {
		real = execAgainstMock(t, "real", realBin, sc, cwdR, homeR)
		ours = execAgainstMock(t, "ours", binPath, sc, cwdO, homeO)
	}

	var rep strings.Builder
	fmt.Fprintf(&rep, "\n=== DIFF REPORT: %s ===\n", sc.name)
	fmt.Fprintf(&rep, "real: %s  exit=%d  reqs=%d\n  hits: %s\n",
		realBin, real.code, len(real.bodies), hitsLine(real.hits))
	fmt.Fprintf(&rep, "ours: %s  exit=%d  reqs=%d\n  hits: %s\n",
		binPath, ours.code, len(ours.bodies), hitsLine(ours.hits))

	for _, side := range []diffRun{real, ours} {
		if side.timedOut {
			t.Errorf("%s: %s timed out\nstdout tail: %s\nstderr tail: %s",
				sc.name, side.label, truncStr(side.stdout, 1500), truncStr(side.stderr, 1500))
		}
		if side.code != 0 && !sc.interactive {
			t.Errorf("%s: %s exit=%d\nstdout tail: %s\nstderr tail: %s",
				sc.name, side.label, side.code, truncStr(side.stdout, 1500), truncStr(side.stderr, 1500))
		}
		if len(side.bodies) == 0 {
			t.Errorf("%s: %s sent no /v1/messages requests", sc.name, side.label)
		}
	}

	total := 0
	n := len(real.bodies)
	if len(ours.bodies) > n {
		n = len(ours.bodies)
	}
	for i := 0; i < n; i++ {
		fmt.Fprintf(&rep, "request[%d]:\n", i)
		switch {
		case i >= len(real.bodies):
			fmt.Fprintf(&rep, "  real=<no request> ours=%d bytes\n", len(ours.bodies[i]))
			total++
		case i >= len(ours.bodies):
			fmt.Fprintf(&rep, "  real=%d bytes ours=<no request>\n", len(real.bodies[i]))
			total++
		default:
			var a, b any
			if err := json.Unmarshal(normalizeWire(real.bodies[i], real), &a); err != nil {
				t.Fatalf("%s: real req[%d] not JSON: %v", sc.name, i, err)
			}
			if err := json.Unmarshal(normalizeWire(ours.bodies[i], ours), &b); err != nil {
				t.Fatalf("%s: ours req[%d] not JSON: %v", sc.name, i, err)
			}
			// safeguards is GB-flag/auto-mode gated in real — verified
			// nondeterministic across same-env runs (say-hi had it,
			// tool-loop didn't). Compare its shape when both send it;
			// presence alone isn't a divergence.
			absorbConditional(a, b)
			d := &differ{}
			d.value("body", a, b)
			if i < len(real.headers) && i < len(ours.headers) {
				diffHeaders(d, real.headers[i], ours.headers[i])
			}
			if len(d.out) == 0 {
				fmt.Fprintln(&rep, "  <identical after normalization>")
			}
			for _, l := range d.out {
				rep.WriteString(l + "\n")
			}
			total += len(d.out)
			if len(d.out) >= maxFieldDiffs {
				fmt.Fprintf(&rep, "  … diff truncated at %d fields\n", maxFieldDiffs)
			}
		}
	}
	fmt.Fprintf(&rep, "=== %s: %d field diffs, real=%d reqs ours=%d reqs ===\n",
		sc.name, total, len(real.bodies), len(ours.bodies))
	fmt.Print(rep.String())
}
