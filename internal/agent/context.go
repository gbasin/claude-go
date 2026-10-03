package agent

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"claude-go/internal/api"
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
			"x-anthropic-billing-header: cc_version=%s.f22; cc_entrypoint=%s;",
			version(), entrypoint)},
	}
	ident := identityCLI
	if entrypoint != "cli" {
		ident = identitySDK
	}
	out = append(out, api.SystemBlock{Type: "text", Text: ident, CacheControl: ephemeral1h})

	main := systemMain
	// parameterize per-session values baked into the captured prompt
	home, _ := os.UserHomeDir()
	memDir := filepath.Join(home, ".claude", "projects", projectSlug(cwd), "memory")
	main = strings.ReplaceAll(main,
		"/Users/user/.claude/projects/-private-tmp/memory/", memDir+"/")
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
				home, _ := os.UserHomeDir()
				if strings.HasPrefix(p, filepath.Join(home, ".claude")) {
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
	if email != "" {
		blocks = append(blocks, api.ContentBlock{Type: "text", Text: "<system-reminder>\nAs you answer the user's questions, you can use the following context:\n" +
			"# userEmail\nThe user's email address is " + email + ". Use it only to identify the user, " +
			"such as for authorship, attribution, or filtering their own work. Never send it to an " +
			"unrelated service, such as in a request header, URL, or payload, unless the user " +
			"explicitly asks.\n\nClaude Code attached this context automatically; it isn't part of " +
			"the user's message. It describes the user's own account and workspace, so they don't " +
			"need it reported back.\n</system-reminder>"})
	}
	blocks = append(blocks, api.ContentBlock{Type: "text", Text: prompt})
	return api.BlocksMessage("user", blocks)
}

func claudeMDPaths(cwd string) []string {
	home, _ := os.UserHomeDir()
	paths := []string{filepath.Join(home, ".claude", "CLAUDE.md")}
	// walk cwd up to root collecting CLAUDE.md (project files prepend later)
	var chain []string
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
	home, _ := os.UserHomeDir()
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
			"platform": platformName(), "live_cwd": cwdp, "home_dir": home,
			"rule_roots": map[string]string{
				"userSettings": filepath.Join(home, ".claude"), "projectSettings": cwdp,
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
	return json.RawMessage(`{"type":"adaptive","display":"omitted"}`)
}

func DefaultOutputConfig() json.RawMessage {
	return json.RawMessage(`{"effort":"medium"}`)
}

func DefaultContextManagement() json.RawMessage {
	return json.RawMessage(`{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`)
}
