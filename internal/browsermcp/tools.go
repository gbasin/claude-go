package browsermcp

import (
	_ "embed"
	"encoding/json"
	"strings"
)

// Tool surface — verbatim from the Claude Code 2.1.288 bundle:
//
//	mod_0801.js — FTe: the 22-entry tool array (names, descriptions,
//	              inputSchemas, _meta) the server hands to tools/list.
//	mod_0107.js — dc ("claude-in-chrome" server name), cTn, dTn (tool
//	              order), y7o, uTn, gme (dynamic stdio config), jXr
//	              (config sniffing for the flag).
//	mod_0803.js — D(e): FTe filtered by transport (oko = bridge-only
//	              tools) and context.excludedTools; the
//	              list_connected_browsers description gets sko() appended.

//go:embed tools.json
var toolsJSON []byte

// ServerName is the MCP server registration name (mod_0107.js dc).
const ServerName = "claude-in-chrome"

// ServerDisplayName is the MCP serverInfo.name (mod_0807.js K()).
const ServerDisplayName = "Claude in Chrome"

// FlagClaudeInChromeMCP is the stdio-server argv flag (mod_0107.js t).
const FlagClaudeInChromeMCP = "--claude-in-chrome-mcp"

// FlagChromeNativeHost is the native-messaging-host argv flag
// (mod_0006.js: "--chrome-native-host" → runChromeNativeHost).
const FlagChromeNativeHost = "--chrome-native-host"

// MCPToolPrefix is the in-session tool prefix (mod_0233.js FD).
const MCPToolPrefix = "mcp__" + ServerName + "__"

// bridgeOnlyTools is mod_0801.js oko — these tools need the WebSocket
// bridge and are dropped from tools/list when serving over a socket.
var bridgeOnlyTools = map[string]bool{
	"switch_browser":          true,
	"list_connected_browsers": true,
	"select_browser":          true,
}

// noBatchTools is mod_0801.js bne — tools that must not appear inside a
// browser_batch actions array (kept for parity with the bundled list).
var noBatchTools = map[string]bool{
	"tabs_context_mcp": true, "tabs_create_mcp": true, "tabs_close_mcp": true,
	"shortcuts_list": true, "shortcuts_execute": true, "resize_window": true,
	"switch_browser": true, "list_connected_browsers": true, "gif_creator": true,
}

// ErrFileUploadUnavailable is mod_0107.js uTn — emitted by the host when a
// session can't satisfy file_upload paths (surfaced here for callers that
// embed the tool surface).
const ErrFileUploadUnavailable = "file_upload is not available in this session, alone or inside browser_batch, and nothing in this call ran. Ask the user to choose the file in their browser themselves."

// switchBrowserOption is mod_0801.js a — the final option label real puts
// in every "pick a browser" question.
const switchBrowserOption = "Open a confirmation screen in every connected Chrome extension and let me select the right one there."

// Tool is one tools/list entry.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Meta        json.RawMessage `json:"_meta,omitempty"`
}

// allTools is the raw FTe array (22 entries).
var allTools = func() []Tool {
	var t []Tool
	if err := json.Unmarshal(toolsJSON, &t); err != nil {
		panic("browsermcp: embedded tools.json: " + err.Error())
	}
	return t
}()

// toolNamesOrdered mirrors mod_0107.js dTn (the order the CLI surfaces
// them, identical to FTe order).
var toolNamesOrdered = []string{
	"javascript_tool", "read_page", "find", "form_input", "computer",
	"browser_batch", "navigate", "resize_window", "gif_creator",
	"upload_image", "get_page_text", "tabs_context_mcp", "tabs_create_mcp",
	"tabs_close_mcp", "read_console_messages", "read_network_requests",
	"shortcuts_list", "shortcuts_execute", "file_upload",
	"switch_browser", "list_connected_browsers", "select_browser",
}

// askUserToolSuffix mirrors mod_0801.js sko(e.askUserToolName) — appended
// to the list_connected_browsers description at tools/list time.
func askUserToolSuffix(askTool string) string {
	var tool string
	if askTool != "" {
		tool = "the " + askTool + " tool"
	} else {
		tool = "your ask-user tool (if available)"
	}
	return `You do not need to call this before using the browser: when one browser is connected, or one was already chosen for this session, browser tools just work. Only if a browser tool reports that several browsers are connected and none is selected, or the user asks to change browsers, ask with ` + tool + `: one option per connected browser, the ones on this computer first (display name as the label, deviceId in parentheses), plus a final option labeled exactly: "` + switchBrowserOption + `" Then call select_browser with the chosen deviceId, or switch_browser for the final option. Never pick one yourself.`
}

// pickToolQuestion mirrors mod_0801.js C2n — the instruction text embedded
// in multi-browser error results.
func pickToolQuestion(askTool string) string {
	var tool string
	if askTool != "" {
		tool = "the " + askTool + " tool"
	} else {
		tool = "your ask-user tool (if available)"
	}
	return `Before any browser action, you MUST call ` + tool + ` with a question listing EVERY connected browser as a separate option (use the display name as the label, and include the deviceId in parentheses), plus one final option labeled exactly: "` + switchBrowserOption + `" Do not skip any connected browser and do not pick one yourself. If the user picks a specific browser, call select_browser with that browser's deviceId. ` +
		"If the user picks the final option, call switch_browser — this sends a confirmation prompt to every connected Chrome extension and waits for the user to click Connect in the one they want; it also lets them name that browser."
}

// listTools mirrors mod_0803.js D(e) + the tools/list handler: drop
// bridge-only tools when the transport is the local socket, drop
// ctx.ExcludedTools, append the ask-user suffix to
// list_connected_browsers, append hostTools.
func (c *Context) listTools() []Tool {
	out := make([]Tool, 0, len(allTools))
	suffix := askUserToolSuffix(c.AskUserToolName)
	for _, t := range allTools {
		if c.BridgeConfig == nil && bridgeOnlyTools[t.Name] {
			continue
		}
		if c.ExcludedTools[t.Name] {
			continue
		}
		if t.Name == "list_connected_browsers" {
			t.Description = t.Description + " " + suffix
		}
		out = append(out, t)
	}
	out = append(out, c.HostTools()...)
	return out
}

// IsChromeMCPConfig mirrors mod_0107.js jXr — true when an mcpServers
// entry points at this binary's --claude-in-chrome-mcp mode.
func IsChromeMCPConfig(typ, command string, args []string) bool {
	if typ != "" && typ != "stdio" {
		return false
	}
	if strings.Contains(command, FlagClaudeInChromeMCP) {
		return true
	}
	for _, a := range args {
		if strings.Contains(a, FlagClaudeInChromeMCP) {
			return true
		}
	}
	return false
}
