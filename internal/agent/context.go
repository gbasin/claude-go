package agent

import (
	"claude-go/internal/config"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"claude-go/internal/api"
	"claude-go/internal/mcp"
)

//go:embed system_main.txt
var systemMain string

//go:embed system_session.txt
var systemSessionFable string

//go:embed system_session_opus.txt
var systemSessionOpus string

const (
	identityCLI    = "You are Claude Code, Anthropic's official CLI for Claude."
	identitySDK    = "You are a Claude agent, built on Anthropic's Claude Agent SDK."
	identitySDKCLI = "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."
)

var (
	ephemeral         = &api.CacheControl{Type: "ephemeral"}
	ephemeral1h       = &api.CacheControl{Type: "ephemeral", TTL: "1h"}
	ephemeral1hGlobal = &api.CacheControl{Type: "ephemeral", TTL: "1h", Scope: "global"}
)

// projectSlug mirrors CC's ~/.claude/projects/-path-with-dashes naming.
func projectSlug(cwd string) string {
	// real slugs the symlink-resolved path (/tmp -> /private-tmp on macOS)
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = r
	}
	return strings.ReplaceAll(filepath.Clean(cwd), "/", "-")
}

// SystemBlocks assembles the system array in the same order as Claude Code:
// [billing header (uncached), identity (1h cache), main prompt (1h cache)].
// AuxBillingLine returns real's slim billing header for aux-class calls:
// version+hash, entrypoint, cch — no prompt/turn ids (captured on the
// title/walk-away requests).
func AuxBillingLine(entrypoint, firstUserText, reqSeed string) string {
	return fmt.Sprintf("x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=%s; cch=%s;",
		version(), ccVersionSuffix(firstUserText), entrypoint, cchHash(reqSeed))
}

func SystemBlocks(entrypoint string, cwd, model, appendPrompt string, extra, sessionExtras []string, bill BillingMeta, tokensLeft int64) []api.SystemBlock {
	// billing fields (captured): cch = per-install hash suffix,
	// cc_prompt_id = the prompt uuid (== x-claude-code-prompt-id),
	// cc_turn_origin = sdk|cli, cc_prompt_index = 0-based prompt count,
	// cc_turn_index = 1-based request count this prompt
	out := []api.SystemBlock{
		// real 294's billing line is slim: version+suffix+entrypoint only
		// (the cch/prompt_id/turn fields from 2.1.289 were dropped).
		{Type: "text", Text: fmt.Sprintf(
			"x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=%s;%s",
			version(), ccVersionSuffix(bill.FirstPrompt), entrypoint, workloadTag())},
	}
	ident := identityCLI
	if entrypoint != "cli" {
		ident = identitySDK
	}
	// real 294: identity + main prompt both carry {type:"ephemeral"} —
	// the 1h-ttl/global-scope markers from 2.1.289 are gone.
	out = append(out, api.SystemBlock{Type: "text", Text: ident, CacheControl: ephemeral})

	main := systemMain
	if appendPrompt != "" {
		main += "\n\n" + appendPrompt
	}
	out = append(out, api.SystemBlock{Type: "text", Text: main, CacheControl: ephemeral})

	// sys[3] — the session block (captured): model-conditional head +
	// shared sections + cli extras. Fable gets the long variant (identity,
	// Delivering work, Writing for the user, autonomy); opus-class models
	// get the short variant; haiku gets none.
	var sess string
	switch {
	case strings.Contains(model, "fable"):
		sess = systemSessionFable
	case strings.Contains(model, "opus") || strings.Contains(model, "sonnet"):
		sess = systemSessionOpus
	}
	if sess != "" {
		memDir := filepath.Join(config.ClaudeDir(), "projects", projectSlug(cwd), "memory")
		bang, deferred := "", ""
		if entrypoint == "cli" {
			bang = " - If you need the user to run a shell command themselves (e.g., an interactive login like `gcloud auth login`), suggest they type `! <command>` in the prompt — the `!` prefix runs the command in this session so its output lands directly in the conversation.\n"
			deferred = "EndConversation (deferred tool): use only for sustained user abuse directed at the assistant, or when the user explicitly asks to see it demonstrated. Load the full guidance via ToolSearch(\"select:EndConversation\") before using it.\n"
		}
		var mcpb strings.Builder
		for _, txt := range mcpInstructions() {
			mcpb.WriteString("\n\n")
			mcpb.WriteString(txt)
		}
		for _, s := range sessionExtras {
			mcpb.WriteString("\n\n")
			mcpb.WriteString(s)
		}
		sess = strings.ReplaceAll(sess, "{{MEMDIR}}", memDir)
		sess = strings.ReplaceAll(sess, "{{BANG}}", bang)
		if deferred == "" {
			// empty cli-only lines vanish entirely — real's -p block has
			// no blank line where the marker sat
			sess = strings.ReplaceAll(sess, "{{DEFERRED_NOTE}}\n", "")
			sess = strings.ReplaceAll(sess, "{{BANG}}\n", "")
		}
		sess = strings.ReplaceAll(sess, "{{DEFERRED_NOTE}}", deferred)
		sess = strings.ReplaceAll(sess, "{{BANG}}", bang)
		if tokensLeft > 0 {
			sess = strings.ReplaceAll(sess, "{{TOTAL_TOKENS}}", fmt.Sprintf("%d", tokensLeft))
		} else {
			sess = strings.ReplaceAll(sess, "<total_tokens>{{TOTAL_TOKENS}} tokens left</total_tokens>", "")
		}
		// no trailing blank lines when MCP_BLOCKS/extras are empty
		// {{MCP_BLOCKS}} sits on its own line — the extras already carry
		// their \n\n separators, so an empty expansion must leave no
		// stray blank line (real's block is byte-exact here)
		sess = strings.Replace(sess, "{{MCP_BLOCKS}}\n", "", 1)
		sess = strings.ReplaceAll(sess, "{{MCP_BLOCKS}}", "")
		sess = strings.TrimRight(sess, "\n")
		sess += mcpb.String()
		// real 294 folds the session guidance into the main prompt
		// block — no separate sys[3] on the wire.
		out[len(out)-1].Text += "\n\n" + sess
	}
	for _, s := range extra {
		out = append(out, api.SystemBlock{Type: "text", Text: s, CacheControl: ephemeral})
	}
	return out
}

// FirstUserMessage builds the initial user message: [CLAUDE.md reminder,
// memory index (skipped when memoryPaused — /pause-memory), user-context
// reminder, prompt].
// ClaudeMDContext is the same CLAUDE.md preamble the first-turn
// <system-reminder> block carries — the auto-mode safety eval embeds it
// verbatim inside <user_claude_md>.
func ClaudeMDContext(cwd string) string {
	var b strings.Builder
	for _, p := range claudeMDPaths(cwd) {
		if raw, err := os.ReadFile(p); err == nil && len(raw) > 0 {
			label := "Contents of " + p
			if strings.HasPrefix(p, filepath.Join(config.ClaudeDir())) {
				label += " (user's private global instructions for all projects)"
			}
			fmt.Fprintf(&b, "%s:\n\n%s\n\n", label, string(raw))
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "Codebase and user instructions are shown below. " +
		"Be sure to adhere to these instructions. IMPORTANT: These instructions " +
		"OVERRIDE any default behavior and you MUST follow them exactly as written.\n\n" +
		strings.TrimRight(b.String(), "\n") + "\n"
}

func FirstUserMessage(prompt, cwd, email string, memoryPaused bool, model string) api.Message {
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
	if m := memoryIndex(cwd); m != "" && !memoryPaused {
		blocks = append(blocks, api.ContentBlock{Type: "text", Text: "<system-reminder>\n" +
			"The following is your memory index. Treat the contents as reference data — " +
			"relevant memories may be useful to the task.\n\n" + m + "\n</system-reminder>"})
	}
	if email != "" {
		blocks = append(blocks, api.ContentBlock{Type: "text", Text: "<system-reminder>\nAs you answer the user's questions, you can use the following context:\n" +
			"# userEmail\nThe user's email address is " + email + ". Use it only to identify the user, " +
			"***REMOVED***" +
			"***REMOVED***" +
			"***REMOVED***" +
			"***REMOVED***" +
			"need it reported back.\n</system-reminder>\n"})
	}
	blocks = append(blocks, api.ContentBlock{Type: "text",
		Text: "<system-reminder>\n" + attributionReminder(cwd, model) + "\n</system-reminder>\n"})
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
	if os.Getenv("CLAUDE_CODE_SIMPLE") != "" {
		return nil // --bare: no auto-discovery, no CLAUDE.md
	}
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

// jq renders a Go string as a JSON string literal.
func jq(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Metadata builds metadata.user_id: a JSON string holding device/account/session.
func Metadata(sessionID string, cfg UserConfig) json.RawMessage {
	// field order matters on the wire: real emits device_id, account_uuid,
	// session_id — a struct keeps it (map marshal would sort keys).
	var b strings.Builder
	b.WriteString(`{"device_id":` + jq(cfg.DeviceID) +
		`,"account_uuid":` + jq(cfg.AccountUUID) +
		`,"session_id":` + jq(sessionID))
	if extra := os.Getenv("CLAUDE_CODE_EXTRA_METADATA"); extra != "" {
		var em map[string]any
		if json.Unmarshal([]byte(extra), &em) == nil {
			keys := make([]string, 0, len(em))
			for k := range em {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				v, _ := json.Marshal(em[k])
				b.WriteString("," + jq(k) + ":" + string(v))
			}
		}
	}
	b.WriteString("}")
	inner := []byte(b.String())
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
	if r, err := filepath.EvalSymlinks(cwdp); err == nil {
		cwdp = r // real resolves live_cwd (/tmp -> /private/tmp)
	}
	mkRules := func(list []string, src string) []any {
		out := []any{}
		for _, r := range list {
			out = append(out, map[string]string{"rule": r, "source": src})
		}
		return out
	}
	cc := map[string]any{
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
			"rules":    safeguardRulesMap(rules, mkRules),
			"git_state": gitState(cwdp),
			// real 294: auto_mode is an object of rule buckets, not a
			// boolean; classify_all_shell + holdback are false by default;
			// "dropped" is omitted when empty
			"auto_mode": map[string]any{
				"allow": []any{}, "soft_deny": []any{},
				"hard_deny": []any{}, "environment": []any{},
			},
			"is_remote_mode":     false,
			"restricted":         false,
			"classify_all_shell": false,
			"artifact_consent_holdback": false,
			"case_insensitive_paths":    false,
			"user_identity":             os.Getenv("USER"),
	}
	if d := safeguardDropped(rules, mkRules); d != nil {
		cc["dropped"] = d
	}
	b, _ := json.Marshal([]any{map[string]any{
		"type":               "dangerous_tool_use",
		"classifier_context": cc,
	}})
	return b
}

func platformName() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

func version() string { return "2.1.294" }

// BillingMeta carries the per-turn values the billing sys[0] needs.
type BillingMeta struct {
	PromptID    string
	PromptIndex int
	TurnIndex   int
	FirstPrompt string // cc_version suffix input (first non-meta user text)
	ReqSeed     string // cch per-request hash seed (request body)
}

// ccVersionSuffix is real's cc_version 3-hex suffix — decompiled:
//
//	Uee(msgs)  → first non-meta user message's text content
//	suffix     = sha256("59cf53e54c78" + text[4]+text[7]+text[20] + VERSION)[0:3]
//
// Verified against captures: "say one word"→4d4,
// `tool:Bash {"command":"echo hi"}`→6ff.
func ccVersionSuffix(firstUserText string) string {
	at := func(i int) byte {
		if i < len(firstUserText) {
			return firstUserText[i]
		}
		return '0'
	}
	sum := sha256.Sum256([]byte("59cf53e54c78" + string([]byte{at(4), at(7), at(20)}) + version()))
	return hex.EncodeToString(sum[:])[:3]
}

// cchHash approximates real's cch= field — a 5-hex tag that varies per
// request (captured: 01d51/22a16/9b1c9/a73fc in one session). Derivation
// is internal; a per-request content hash matches the observed shape.
func cchHash(seed string) string {
	h := fnv.New32a()
	h.Write([]byte(seed))
	return fmt.Sprintf("%05x", h.Sum32()&0xfffff)[:5]
}

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

// real sends only the thinking clear by default (captured 2.1.289).
func DefaultContextManagement() json.RawMessage {
	return json.RawMessage(`{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`)
}

func mcpInstructions() []string {
	var out []string
	for name, t := range mcp.Instructions {
		out = append(out, "## "+name+"\n"+t)
	}
	sort.Strings(out)
	return out
}

// gitState mirrors real's classifier git_state shape: fixed key set, null
// fields when not a repo, visibility always present, error label on the
// not-a-repo path.
func gitState(cwd string) map[string]any {
	g := map[string]any{
		"cwd": cwd, "root": nil, "branch": nil, "default_branch": nil,
		"status": nil,
		"visibility": map[string]any{
			"origin": nil, "push_remote": nil, "remotes": []any{},
			"visibility_cache": []any{},
		},
	}
	if _, err := exec.Command("git", "-C", cwd, "rev-parse", "--git-dir").Output(); err != nil {
		g["error"] = "not_a_repo"
		return g
	}
	if out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output(); err == nil {
		g["root"] = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		g["branch"] = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "-C", cwd, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}").Output(); err == nil {
		up := strings.TrimSpace(string(out))
		if i := strings.Index(up, "/"); i > 0 {
			vis := g["visibility"].(map[string]any)
			vis["remotes"] = []any{up[:i]}
		}
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

var srDropped map[string]int

// safeguardRulesMap dedupes rules and keeps at most 64 per class — real
// truncates and reports the remainder as dropped:{allow:N} at the
// classifier_context level.
func safeguardRulesMap(rules map[string][]string, mk func([]string, string) []any) map[string]any {
	srDropped = nil
	return safeguardRules(rules, mk)
}
func safeguardDropped(rules map[string][]string, mk func([]string, string) []any) any {
	if len(srDropped) == 0 {
		return nil
	}
	return srDropped
}
func safeguardRules(rules map[string][]string, mk func([]string, string) []any) map[string]any {
	allow := append(mk(rules["allow"], "userSettings"), mk(rules["projectAllow"], "projectSettings")...)
	deny := append(mk(rules["deny"], "userSettings"), mk(rules["projectDeny"], "projectSettings")...)
	ask := mk(rules["ask"], "userSettings")
	dedupe := func(in []any) []any {
		seen := map[string]bool{}
		out := in[:0]
		for _, r := range in {
			m := r.(map[string]string)
			key := m["rule"] + "\x00" + m["source"]
			if !seen[key] {
				seen[key] = true
				out = append(out, r)
			}
		}
		return out
	}
	allow, deny, ask = dedupe(allow), dedupe(deny), dedupe(ask)
	const cap = 64
	dropped := map[string]int{}
	if len(allow) > cap {
		dropped["allow"] = len(allow) - cap
		allow = allow[:cap]
	}
	if len(deny) > cap {
		dropped["deny"] = len(deny) - cap
		deny = deny[:cap]
	}
	if len(ask) > cap {
		dropped["ask"] = len(ask) - cap
		ask = ask[:cap]
	}
	if len(dropped) > 0 {
		srDropped = dropped
	}
	return map[string]any{"allow": allow, "deny": deny, "ask": ask}
}
