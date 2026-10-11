// Package extract provides runtime access to Claude Code's bundled text
// (system prompts, tool schemas) harvested from a local `claude` install
// instead of vendored into this repo. `claude-go extract` drives real
// claude against the in-process mock API, captures the /v1/messages
// request, and caches the text in ~/.claude-go/extracted/. Consumers fall
// back to minimal community-authored text when no extraction exists.
package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"claude-go/internal/mock"
)

// Dir is the extraction cache root — ~/.claude-go/extracted, overridable
// for tests via CLAUDE_GO_EXTRACTED_DIR.
func Dir() string {
	if d := os.Getenv("CLAUDE_GO_EXTRACTED_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude-go", "extracted")
}

// Available reports whether an extraction exists.
func Available() bool {
	_, err := os.Stat(filepath.Join(Dir(), "meta.json"))
	return err == nil
}

// Meta records the extraction provenance.
type Meta struct {
	ClaudeVersion string    `json:"claude_version"`
	HarvestedAt   time.Time `json:"harvested_at"`
	Models        []string  `json:"models"` // model classes captured
}

// ReadMeta returns the extraction provenance, if any.
func ReadMeta() (*Meta, error) {
	b, err := os.ReadFile(filepath.Join(Dir(), "meta.json"))
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Text reads an extracted text file (e.g. "system_main_fable.txt").
func Text(name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(Dir(), name))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// JSON reads an extracted JSON file (e.g. "tools.json").
func JSON(name string) (json.RawMessage, bool) {
	b, err := os.ReadFile(filepath.Join(Dir(), name))
	if err != nil {
		return nil, false
	}
	return b, true
}

// Render substitutes the extraction-time tokens back to runtime values.
// Extraction tokenizes values that were known in the harvest sandbox
// (cwd, home, memdir, user, date); Render reverses that for the live env.
func Render(text, cwd string) string {
	home, _ := os.UserHomeDir()
	out := text
	out = strings.ReplaceAll(out, "{{MEMDIR}}",
		filepath.Join(configDir(), "projects", projectSlug(cwd), "memory"))
	out = strings.ReplaceAll(out, "{{CWD}}", cwd)
	out = strings.ReplaceAll(out, "{{HOME}}", home)
	out = strings.ReplaceAll(out, "{{USER}}", os.Getenv("USER"))
	out = strings.ReplaceAll(out, "{{DATE}}", time.Now().Format("2006-01-02"))
	return out
}

func configDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// projectSlug mirrors CC's ~/.claude/projects/-path-with-dashes naming.
func projectSlug(cwd string) string {
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = r
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, filepath.Clean(cwd))
}

// --- harvest -----------------------------------------------------------------

// wireBody is the captured /v1/messages request.
type wireBody struct {
	Model             string            `json:"model"`
	MaxTokens         int               `json:"max_tokens"`
	System            []json.RawMessage `json:"system"`
	Tools             []json.RawMessage `json:"tools"`
	Messages          []json.RawMessage `json:"messages"`
	Stream            bool              `json:"stream"`
	Thinking          json.RawMessage   `json:"thinking"`
	ContextManagement json.RawMessage   `json:"context_management"`
	Diagnostics       json.RawMessage   `json:"diagnostics"`
}

// HarvestResult summarizes one extraction run.
type HarvestResult struct {
	Version   string
	Models    []string
	SystemLen int
	ToolCount int
	Dir       string
}

// FindRealClaude locates a real claude binary: CLAUDE_REAL_BIN, then
// ~/.local/bin/claude, then PATH.
func FindRealClaude() string {
	if p := os.Getenv("CLAUDE_REAL_BIN"); p != "" {
		return p
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
	return ""
}

// modelClasses are the model ids probed per extraction — real's session
// system block differs per class (fable long, opus/sonnet short, haiku
// none).
var modelClasses = []struct {
	Class string
	Model string
}{
	{"fable", "claude-fable-5-1"},
	{"opus", "claude-opus-5-5"},
	{"haiku", "claude-haiku-4-5-20251001"},
}

// Harvest runs real claude against an in-process mock API per model class
// and writes the extracted text+schemas into Dir().
func Harvest(ctx context.Context, bin string, logf func(string, ...any)) (*HarvestResult, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if bin == "" {
		return nil, fmt.Errorf("no real claude binary found (set CLAUDE_REAL_BIN or install claude)")
	}
	verBytes, _ := exec.CommandContext(ctx, bin, "--version").Output()
	version := strings.TrimSpace(string(verBytes))
	logf("real claude: %s (%s)", bin, version)

	dir := Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	res := &HarvestResult{Version: version, Dir: dir}
	var toolsWritten, skillsWritten bool

	for _, mc := range modelClasses {
		body, raws, headers, mainText, err := capture(ctx, bin, mc.Model, logf)
		if err != nil {
			logf("  %s: capture failed: %v", mc.Class, err)
			continue
		}
		if mainText == "" {
			logf("  %s: no system text captured", mc.Class)
			continue
		}
		fn := fmt.Sprintf("system_main_%s.txt", mc.Class)
		if err := os.WriteFile(filepath.Join(dir, fn), []byte(mainText), 0o644); err != nil {
			return nil, err
		}
		res.Models = append(res.Models, mc.Class)
		res.SystemLen += len(mainText)
		if !toolsWritten && len(body.Tools) > 0 {
			if err := writeJSONLines(filepath.Join(dir, "tools.json"), body.Tools); err != nil {
				return nil, err
			}
			res.ToolCount = len(body.Tools)
			toolsWritten = true
		}
		writeWireCapture(dir, "print_"+mc.Class, raws[len(raws)-1], headers)
		if !skillsWritten {
			if idx := skillsIndexText(body); idx != "" {
				os.WriteFile(filepath.Join(dir, "skills_index.txt"),
					[]byte(idx), 0o644)
				skillsWritten = true
			}
		}
		if idx := envSectionText(body, agentsMarker); idx != "" {
			os.WriteFile(filepath.Join(dir, "agents_index_sdk.txt"),
				[]byte(idx), 0o644)
		}
	}
	// one default-model run feeds the golden-wire fixture — real's default
	// is not necessarily a probed class
	if _, raws, headers, _, err := capture(ctx, bin, "", logf); err == nil {
		writeWireCapture(dir, "print_default", raws[len(raws)-1], headers)
	}

	// tool-loop run — the golden toolloop fixture is the second request
	// (post-tool_use); mock serves "tool:<name> <json>" prompts
	if raws, hdrs, err := captureArgv(ctx, bin, "-p", `tool:Bash {"command":"echo hi"}`,
		"--permission-mode", "bypassPermissions"); err == nil && len(raws) > 1 {
		writeWireCapture(dir, "toolloop_default", raws[1], hdrs)
	}

	// interactive (PTY) run — the cli entrypoint's wire differs: CLI tool
	// schema variants and a session block with interactive-only lines
	if body, rawBody, headers, cliMain, err := captureInteractive(ctx, bin, logf); err == nil {
		if cliMain != "" {
			os.WriteFile(filepath.Join(dir,
				"system_main_cli_"+cliModelClass(body.Model)+".txt"),
				[]byte(cliMain), 0o644)
		}
		if len(body.Tools) > 0 {
			writeJSONLines(filepath.Join(dir, "tools_cli.json"), body.Tools)
		}
		if idx := envSectionText(body, agentsMarker); idx != "" {
			os.WriteFile(filepath.Join(dir,
				"agents_index_cli_"+cliModelClass(body.Model)+".txt"),
				[]byte(idx), 0o644)
		}
		writeWireCapture(dir, "print_interactive", rawBody, headers)
	} else {
		logf("  interactive: capture skipped: %v", err)
	}
	if len(res.Models) == 0 {
		return nil, fmt.Errorf("no captures succeeded")
	}
	meta, _ := json.MarshalIndent(&Meta{
		ClaudeVersion: version, HarvestedAt: time.Now(), Models: res.Models,
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o644); err != nil {
		return nil, err
	}
	return res, nil
}

// capture runs `claude -p ping --model X` against a fresh mock in a seeded
// sandbox. Returns the last /v1/messages body, its headers, and the main
// system text with run-known dynamic values tokenized.
func capture(ctx context.Context, bin, model string, logf func(string, ...any)) (*wireBody, [][]byte, http.Header, string, error) {
	srv, err := mock.Start()
	if err != nil {
		return nil, nil, nil, "", err
	}
	defer srv.Shutdown(ctx)

	home, cwd, env, err := seedSandbox(srv.Addr)
	if err != nil {
		return nil, nil, nil, "", err
	}
	defer os.RemoveAll(home)
	defer os.RemoveAll(cwd)

	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	argv := []string{"-p", "ping"}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	cmd := exec.CommandContext(cctx, bin, argv...)
	cmd.Env = env
	cmd.Dir = cwd
	out, runErr := cmd.CombinedOutput()

	if len(srv.Bodies) == 0 {
		return nil, nil, nil, "", fmt.Errorf("real claude made no API request: %v\n%s", runErr, out)
	}
	raw := srv.Bodies[len(srv.Bodies)-1]
	var b wireBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, nil, nil, "", fmt.Errorf("captured body unmarshal: %w", err)
	}
	return &b, srv.Bodies, srv.Headers[len(srv.Bodies)-1], tokenize(mainSystemText(b.System), cwd, home), nil
}

// seedSandbox makes the isolated HOME/cwd + env real claude runs under.
func seedSandbox(mockAddr string) (home, cwd string, env []string, err error) {
	home, err = os.MkdirTemp("", "claude-go-extract-home")
	if err != nil {
		return "", "", nil, err
	}
	home, _ = filepath.EvalSymlinks(home)
	cwd, err = os.MkdirTemp("", "claude-go-extract-cwd")
	if err != nil {
		return "", "", nil, err
	}
	cwd, _ = filepath.EvalSymlinks(cwd)

	// seed onboarding + project trust so real claude doesn't prompt
	claudeJSON := map[string]any{
		"hasCompletedOnboarding": true,
		"projects":               map[string]any{cwd: map[string]any{"hasTrustDialogAccepted": true}},
	}
	if b, err := json.Marshal(claudeJSON); err == nil {
		os.WriteFile(filepath.Join(home, ".claude.json"), b, 0o644)
	}
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.MkdirAll(filepath.Join(home, ".tmp"), 0o755)

	env = []string{
		"HOME=" + home, "SHELL=/bin/bash", "TERM=xterm-256color",
		"LANG=en_US.UTF-8", "TMPDIR=" + filepath.Join(home, ".tmp"),
		"USER=" + os.Getenv("USER"), "PATH=" + os.Getenv("PATH"),
		"ANTHROPIC_BASE_URL=" + mockAddr,
		"ANTHROPIC_AUTH_TOKEN=test", "ANTHROPIC_API_KEY=test",
		"CLAUDE_CODE_OAUTH_TOKEN=test",
		"DISABLE_TELEMETRY=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
	return home, cwd, env, nil
}

// captureInteractive runs real claude on a PTY (interactive/cli
// entrypoint — different tool schemas than -p) and types one turn.
func captureInteractive(ctx context.Context, bin string, logf func(string, ...any)) (*wireBody, []byte, http.Header, string, error) {
	srv, err := mock.Start()
	if err != nil {
		return nil, nil, nil, "", err
	}
	defer srv.Shutdown(ctx)

	home, cwd, env, err := seedSandbox(srv.Addr)
	if err != nil {
		return nil, nil, nil, "", err
	}
	defer os.RemoveAll(home)
	defer os.RemoveAll(cwd)

	master, slave, err := ptyAlloc(120, 40)
	if err != nil {
		return nil, nil, nil, "", err
	}
	defer master.Close()

	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin)
	cmd.Env = env
	cmd.Dir = cwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = ptyProcAttr()
	if err := cmd.Start(); err != nil {
		slave.Close()
		return nil, nil, nil, "", fmt.Errorf("start: %w", err)
	}
	slave.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	go io.Copy(io.Discard, master) // drain TUI output

	// type a turn once the REPL is up — a couple of retries covers slow boots
	go func() {
		for i := 0; i < 12; i++ {
			time.Sleep(2 * time.Second)
			master.Write([]byte("ping\r"))
		}
	}()

	deadline := time.Now().Add(60 * time.Second)
	for len(srv.Bodies) == 0 && time.Now().Before(deadline) {
		select {
		case err := <-done:
			return nil, nil, nil, "", fmt.Errorf("real claude exited before any request: %v", err)
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}
	cmd.Process.Kill()
	<-done
	if len(srv.Bodies) == 0 {
		return nil, nil, nil, "", fmt.Errorf("no request captured in interactive run")
	}
	raw := srv.Bodies[len(srv.Bodies)-1]
	var b wireBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, nil, nil, "", fmt.Errorf("interactive body unmarshal: %w", err)
	}
	return &b, raw, srv.Headers[len(srv.Bodies)-1], tokenize(mainSystemText(b.System), cwd, home), nil
}

func mainSystemText(system []json.RawMessage) string {
	var out string
	for _, raw := range system {
		var blk struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &blk) != nil || blk.Text == "" {
			continue
		}
		out = blk.Text
	}
	return out
}

// tokenize replaces run-known dynamic values with {{TOKEN}} markers so the
// stored text is environment-independent.
func tokenize(text, cwd, home string) string {
	out := text
	// memdir first — it contains both home and cwd as substrings
	memDir := filepath.Join(home, ".claude", "projects", projectSlug(cwd), "memory")
	out = strings.ReplaceAll(out, memDir, "{{MEMDIR}}")
	out = strings.ReplaceAll(out, cwd, "{{CWD}}")
	out = strings.ReplaceAll(out, home, "{{HOME}}")
	if u := os.Getenv("USER"); u != "" {
		out = strings.ReplaceAll(out, u, "{{USER}}")
	}
	out = strings.ReplaceAll(out, time.Now().Format("2006-01-02"), "{{DATE}}")
	return out
}

// system text with run-known dynamic values tokenized.
func writeJSONLines(path string, items []json.RawMessage) error {
	var b strings.Builder
	b.WriteString("[\n")
	for i, it := range items {
		b.Write(it)
		if i < len(items)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("]\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// wireCapture mirrors the e2e golden fixture shape.
type wireCapture struct {
	RequestLine string            `json:"request_line"`
	Headers     map[string]string `json:"headers"`
	Body        json.RawMessage   `json:"body"`
}

func writeWireCapture(dir, name string, raw []byte, headers http.Header) {
	h := map[string]string{}
	for k, v := range headers {
		if len(v) > 0 {
			h[strings.ToLower(k)] = v[0]
		}
	}
	wc := wireCapture{
		RequestLine: "POST /v1/messages?beta=true HTTP/1.1",
		Headers:     h, Body: raw,
	}
	b, _ := json.MarshalIndent(wc, "", "  ")
	os.WriteFile(filepath.Join(dir, "realwire_"+name+".json"), b, 0o644)
}

const skillsMarker = "The following skills are available for use with the Skill tool:"
const agentsMarker = "Available agent types for the Agent tool:"

func skillsIndexText(b *wireBody) string { return envSectionText(b, skillsMarker) }

// envSectionText returns the "- name: desc" index under marker in the
// env message — entries plus their non-dash continuation lines; the index
// ends at a blank line followed by a non-dash line (the next section).
func envSectionText(b *wireBody, marker string) string {
	for _, m := range b.Messages {
		var msg struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(m, &msg) != nil {
			continue
		}
		for _, c := range msg.Content {
			i := strings.Index(c.Text, marker)
			if i < 0 {
				continue
			}
			rest := c.Text[i+len(marker):]
			// entries are "- name: desc" plus non-dash continuation
			// lines; the index ends at a blank line followed by a
			// non-dash line (the next section starts there)
			raw := strings.Split(rest, "\n")
			var lines []string
			for j, ln := range raw {
				if strings.HasPrefix(ln, "- ") {
					lines = append(lines, ln)
					continue
				}
				if strings.TrimSpace(ln) == "" {
					if len(lines) == 0 {
						continue // blank lines before the first entry
					}
					// lookahead: blank before a non-dash line ends it
					end := true
					for _, nxt := range raw[j+1:] {
						if strings.TrimSpace(nxt) == "" {
							continue
						}
						end = !strings.HasPrefix(nxt, "- ")
						break
					}
					if end {
						break
					}
					continue
				}
				if len(lines) > 0 {
					lines = append(lines, ln) // continuation of prev entry
					continue
				}
				break
			}
			if len(lines) > 0 {
				return strings.Join(lines, "\n") + "\n"
			}
		}
	}
	return ""
}

// cliModelClass buckets a model id for the interactive capture filename.
func cliModelClass(model string) string {
	switch {
	case strings.Contains(model, "fable"):
		return "fable"
	case strings.Contains(model, "opus"), strings.Contains(model, "sonnet"):
		return "opus"
	default:
		return "haiku"
	}
}

// captureArgv runs real claude with an arbitrary argv in the seeded
// sandbox and returns all captured request bodies + last-seen headers.
func captureArgv(ctx context.Context, bin string, argv ...string) ([][]byte, http.Header, error) {
	srv, err := mock.Start()
	if err != nil {
		return nil, nil, err
	}
	defer srv.Shutdown(ctx)

	home, cwd, env, err := seedSandbox(srv.Addr)
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(home)
	defer os.RemoveAll(cwd)

	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, argv...)
	cmd.Env = env
	cmd.Dir = cwd
	out, runErr := cmd.CombinedOutput()

	if len(srv.Bodies) == 0 {
		return nil, nil, fmt.Errorf("no API request: %v\n%s", runErr, out)
	}
	return srv.Bodies, srv.Headers[len(srv.Bodies)-1], nil
}
