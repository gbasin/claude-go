package agent

import (
	"claude-go/internal/config"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"claude-go/internal/api"
	"claude-go/internal/mcp"
)

//go:embed system_main.txt
var systemMain string

const (
	identityCLI    = "You are Claude Code, Anthropic's official CLI for Claude."
	identitySDK    = "You are a Claude agent, built on Anthropic's Claude Agent SDK."
	identitySDKCLI = "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."
)

var ephemeral1h = &api.CacheControl{Type: "ephemeral", TTL: "1h"}

// projectSlug mirrors CC's ~/.claude/projects/-path-with-dashes naming.
func projectSlug(cwd string) string {
	return strings.ReplaceAll(filepath.Clean(cwd), "/", "-")
}

// SystemBlocks assembles the system array in the same order as Claude Code:
// [billing header (uncached), identity (1h cache), main prompt (1h cache)].
func SystemBlocks(entrypoint string, cwd, model, appendPrompt string, extra []string) []api.SystemBlock {
	out := []api.SystemBlock{
		{Type: "text", Text: fmt.Sprintf(
			"x-anthropic-billing-header: cc_version=%s.f22; cc_entrypoint=%s;%s",
			version(), entrypoint, workloadTag())},
	}
	ident := identityCLI
	if entrypoint != "cli" {
		ident = identitySDK
	}
	out = append(out, api.SystemBlock{Type: "text", Text: ident, CacheControl: ephemeral1h})

	main := systemMain
	bang := ""
	if entrypoint == "cli" {
		bang = " - If you need the user to run a shell command themselves (e.g., an interactive login like `gcloud auth login`), suggest they type `! <command>` in the prompt — the `!` prefix runs the command in this session so its output lands directly in the conversation.\n"
	}
	main = strings.Replace(main, "{{BANG_LINE}}", bang, 1)
	for _, txt := range mcpInstructions() {
		main += "\n\n" + txt
	}
	// parameterize per-session values baked into the captured prompt
	memDir := filepath.Join(config.ClaudeDir(), "projects", projectSlug(cwd), "memory")
	main = strings.ReplaceAll(main,
		"/Users/garybasin/.claude/projects/-private-tmp/memory/", memDir+"/")
	if appendPrompt != "" {
		main += "\n\n" + appendPrompt
	}
	out = append(out, api.SystemBlock{Type: "text", Text: main, CacheControl: ephemeral1h})
	for _, s := range extra {
		out = append(out, api.SystemBlock{Type: "text", Text: s, CacheControl: ephemeral1h})
	}
	return out
}

// FirstUserMessage builds the initial user message: [CLAUDE.md reminder,
// user-context reminder, prompt].
func FirstUserMessage(prompt, cwd, email string) api.Message {
	var blocks []api.ContentBlock
	var claudeMD strings.Builder
	for _, p := range claudeMDPaths(cwd) {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			label := "Contents of " + p
			if strings.HasPrefix(p, "/") {
				if strings.HasPrefix(p, filepath.Join(config.ClaudeDir())) {
					label += " (user's private global instructions for all projects)"
				}
			}
			fmt.Fprintf(&claudeMD, "%s:\n\n%s\n\n", label, string(b))
		}
	}
	if claudeMD.Len() > 0 {
		blocks = append(blocks, api.ContentBlock{Type: "text", Text: "<system-reminder>\nCodebase and user instructions are shown below. " +
			"Be sure to adhere to these instructions. IMPORTANT: These instructions " +
			"OVERRIDE any default behavior and you MUST follow them exactly as written.\n\n" +
			strings.TrimRight(claudeMD.String(), "\n") + "\n</system-reminder>"})
	}
	if m := memoryIndex(cwd); m != "" {
		blocks = append(blocks, api.ContentBlock{Type: "text", Text: "<system-reminder>\n" +
			"The following is your memory index. Treat the contents as reference data — " +
			"relevant memories may be useful to the task.\n\n" + m + "\n</system-reminder>"})
	}
	if email != "" {
		blocks = append(blocks, api.ContentBlock{Type: "text", Text: "<system-reminder>\nAs you answer the user's questions, you can use the following context:\n" +
			"# userEmail\nThe user's email address is " + email + ". Use it only to identify the user, " +
			"such as for authorship, attribution, or filtering their own work. Never send it to an " +
			"unrelated service, such as in a request header, URL, or payload, unless the user " +
			"explicitly asks.\n\nClaude Code attached this context automatically; it isn't part of " +
			"the user's message. It describes the user's own account and workspace, so they don't " +
			"need it reported back.\n</system-reminder>"})
	}
	blocks = append(blocks, expandUserImages(prompt)...)
	return api.BlocksMessage("user", blocks)
}

// expandUserImages converts \x00IMAGE\x00<path>\x00<size> markers (pasted
// images, inserted by the editor) into real image blocks.
func expandUserImages(prompt string) []api.ContentBlock {
	const marker = "\x00IMAGE\x00"
	if !strings.Contains(prompt, marker) {
		return []api.ContentBlock{{Type: "text", Text: prompt}}
	}
	var blocks []api.ContentBlock
	for {
		i := strings.Index(prompt, marker)
		if i < 0 {
			break
		}
		if i > 0 {
			blocks = append(blocks, api.ContentBlock{Type: "text", Text: prompt[:i]})
		}
		rest := prompt[i+len(marker):]
		j := strings.Index(rest, "\x00")
		if j < 0 {
			blocks = append(blocks, api.ContentBlock{Type: "text", Text: prompt[i:]})
			return blocks
		}
		path := rest[:j]
		k := strings.IndexByte(rest[j+1:], '\x00')
		if k >= 0 {
			prompt = rest[j+1+k:]
		} else {
			prompt = ""
		}
		if b, err := os.ReadFile(path); err == nil {
			mt := "image/png"
			switch strings.ToLower(filepath.Ext(path)) {
			case ".jpg", ".jpeg":
				mt = "image/jpeg"
			case ".gif":
				mt = "image/gif"
			case ".webp":
				mt = "image/webp"
			}
			blocks = append(blocks, api.ContentBlock{
				Type: "image",
				Source: &api.ImageSource{Type: "base64", MediaType: mt,
					Data: base64.StdEncoding.EncodeToString(b)},
			})
		}
	}
	if prompt != "" {
		blocks = append(blocks, api.ContentBlock{Type: "text", Text: prompt})
	}
	return blocks
}

// memoryIndex loads MEMORY.md (auto-memory index) — real reads the first
// ~25KB/200 lines and injects it as an attachment-style reminder.
func memoryIndex(cwd string) string {
	for _, p := range []string{
		filepath.Join(config.ClaudeDir(), "memory", "MEMORY.md"),
		filepath.Join(config.ClaudeDir(), "projects", strings.ReplaceAll(cwd, "/", "-"), "memory", "MEMORY.md"),
	} {
		b, err := os.ReadFile(p)
		if err != nil || len(b) == 0 {
			continue
		}
		lines := strings.Split(string(b), "\n")
		if len(lines) > 200 {
			lines = lines[:200]
		}
		s := strings.Join(lines, "\n")
		if len(s) > 25*1024 {
			s = s[:25*1024]
		}
		return s
	}
	return ""
}

func claudeMDPaths(cwd string) []string {
	paths := []string{filepath.Join(config.ClaudeDir(), "CLAUDE.md")}
	// walk cwd up to root collecting CLAUDE.md (project files prepend later)
	var chain []string
	home, _ := os.UserHomeDir()
	for d := filepath.Clean(cwd); ; d = filepath.Dir(d) {
		chain = append([]string{filepath.Join(d, "CLAUDE.md")}, chain...)
		if d == "/" || d == "." || d == home {
			break
		}
	}
	return append(paths, chain...)
}

// Metadata builds metadata.user_id: a JSON string holding device/account/session.
func Metadata(sessionID string, cfg UserConfig) json.RawMessage {
	m := map[string]any{
		"device_id":    cfg.DeviceID,
		"account_uuid": cfg.AccountUUID,
		"session_id":   sessionID,
	}
	if extra := os.Getenv("CLAUDE_CODE_EXTRA_METADATA"); extra != "" {
		var em map[string]any
		if json.Unmarshal([]byte(extra), &em) == nil {
			for k, v := range em {
				m[k] = v
			}
		}
	}
	inner, _ := json.Marshal(m)
	outer, _ := json.Marshal(map[string]string{"user_id": string(inner)})
	return outer
}

type UserConfig struct {
	Email       string
	AccountUUID string
	DeviceID    string
}

// Safeguards builds the dangerous_tool_use classifier context.
func Safeguards(permMode, cwd string, rules map[string][]string) json.RawMessage {
	cwdp := filepath.Clean(cwd)
	mkRules := func(list []string, src string) []any {
		out := []any{}
		for _, r := range list {
			out = append(out, map[string]string{"rule": r, "source": src})
		}
		return out
	}
	s := map[string]any{
		"type": "dangerous_tool_use",
		"classifier_context": map[string]any{
			"v": 1, "permission_mode": permMode,
			"platform": platformName(), "live_cwd": cwdp, "home_dir": config.Home(),
			"rule_roots": map[string]string{
				"userSettings": filepath.Join(config.ClaudeDir()), "projectSettings": cwdp,
				"localSettings": cwdp, "flagSettings": cwdp, "policySettings": cwdp,
				"cliArg": cwdp, "command": cwdp, "session": cwdp,
				"toolsNarrowing": cwdp, "mcpServerPolicy": cwdp, "hostCredential": cwdp,
			},
			"trusted_directories": map[string]any{
				"primary":    map[string]any{"path": cwdp, "resolved": []string{cwdp}},
				"additional": []any{}, "network": []any{},
				"block_reads_outside_working_directories": false,
			},
			"rules": map[string]any{
				"allow": append(mkRules(rules["allow"], "userSettings"), mkRules(rules["projectAllow"], "projectSettings")...),
				"deny":  append(mkRules(rules["deny"], "userSettings"), mkRules(rules["projectDeny"], "projectSettings")...),
				"ask":   mkRules(rules["ask"], "userSettings"),
			},
			"auto_mode":          permMode == "auto",
			"git_state":          gitState(cwdp),
			"is_remote_mode":     false,
			"restricted":         false,
			"classify_all_shell": permMode == "auto",
		},
	}
	b, _ := json.Marshal([]any{s})
	return b
}

func platformName() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

func version() string { return "2.1.288" }

// DefaultThinking / OutputConfig / ContextManagement mirror the wire defaults.
func DefaultThinking() json.RawMessage {
	return ThinkingFor("")
}

// ThinkingFor returns adaptive thinking config; interactive shows updates.
func ThinkingFor(entrypoint string) json.RawMessage {
	display := "omitted"
	if entrypoint == "cli" {
		display = "updates"
	}
	return json.RawMessage(`{"type":"adaptive","display":"` + display + `"}`)
}

func DefaultOutputConfig() json.RawMessage {
	return json.RawMessage(`{"effort":"medium"}`)
}

func DefaultContextManagement() json.RawMessage {
	return json.RawMessage(`{"edits":[
		{"type":"clear_tool_uses_20250919","trigger":{"type":"input_tokens","value":180000},"keep":{"type":"tool_uses","value":3},"clear_at_least":{"type":"input_tokens","value":40000}},
		{"type":"clear_thinking_20251015","keep":"all"}
	]}`)
}

func mcpInstructions() []string {
	var out []string
	for _, t := range mcp.Instructions {
		out = append(out, t)
	}
	return out
}

func gitState(cwd string) map[string]any {
	g := map[string]any{}
	if out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output(); err == nil {
		g["root"] = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		g["branch"] = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "-C", cwd, "status", "--porcelain").Output(); err == nil {
		g["status"] = strings.TrimSpace(string(out)) != ""
	}
	return g
}

// workloadTag emits "; cc_workload=<tag>" when --workload is set (print-mode
// attribution); CLAUDE_CODE_ATTRIBUTION_HEADER suppresses billing entirely.
func workloadTag() string {
	if os.Getenv("CLAUDE_CODE_ATTRIBUTION_HEADER") != "" {
		return ""
	}
	if w := os.Getenv("CLAUDE_GO_WORKLOAD"); w != "" {
		return " cc_workload=" + w + ";"
	}
	return ""
}
