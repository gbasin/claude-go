package browsermcp

import "encoding/json"

// Community-authored fallbacks replacing the bundled Claude-in-Chrome
// assets. Tool names are functional identifiers; descriptions and the
// prompt are claude-go's own text.

// chromeSystemPrompt is injected as a session block when Chrome tools are
// active.
var chromeSystemPrompt = `You have access to browser automation tools (mcp__claude-in-chrome__*) for interacting with web pages in Chrome.

Load every tool you expect to need in one ToolSearch call when deferred — the select query accepts a comma-separated list. Prefer read_page/get_page_text for content, navigate for URL changes, computer/form_input for interaction, and gif_creator for recordings the user may want to review.`

// fallbackToolsJSON is the MCP tools/list payload: same tool names and
// order as real's surface, with minimal community descriptions and
// permissive object schemas.
var fallbackToolsJSON = mustJSONTools([]Tool{
	{Name: "javascript_tool", Description: "Evaluate JavaScript in the page", InputSchema: objSchema("script")},
	{Name: "read_page", Description: "Read the page's accessibility tree", InputSchema: objSchema(),
		Meta: json.RawMessage(`{"anthropic/maxResultSizeChars":60000}`)},
	{Name: "find", Description: "Find elements on the page", InputSchema: objSchema("query")},
	{Name: "form_input", Description: "Fill a form field", InputSchema: objSchema("selector", "value")},
	{Name: "computer", Description: "Mouse/keyboard interaction (click, type, scroll)", InputSchema: objSchema()},
	{Name: "browser_batch", Description: "Run several browser actions in one call", InputSchema: objSchema("actions")},
	{Name: "navigate", Description: "Navigate the tab to a URL", InputSchema: objSchema("url")},
	{Name: "resize_window", Description: "Resize the browser window", InputSchema: objSchema("width", "height")},
	{Name: "gif_creator", Description: "Record the session as an animated gif", InputSchema: objSchema()},
	{Name: "upload_image", Description: "Upload an image into the page", InputSchema: objSchema("path")},
	{Name: "get_page_text", Description: "Get the page's text content", InputSchema: objSchema()},
	{Name: "tabs_context_mcp", Description: "List open tabs", InputSchema: objSchema()},
	{Name: "tabs_create_mcp", Description: "Open a new tab", InputSchema: objSchema()},
	{Name: "tabs_close_mcp", Description: "Close a tab", InputSchema: objSchema()},
	{Name: "read_console_messages", Description: "Read console output from the page", InputSchema: objSchema()},
	{Name: "read_network_requests", Description: "Read network requests made by the page", InputSchema: objSchema()},
	{Name: "shortcuts_list", Description: "List saved shortcut workflows", InputSchema: objSchema()},
	{Name: "shortcuts_execute", Description: "Run a saved shortcut workflow", InputSchema: objSchema("id")},
	{Name: "file_upload", Description: "Upload a file through a file input", InputSchema: objSchema("selector", "path")},
	{Name: "switch_browser", Description: "Switch to a different connected browser", InputSchema: objSchema()},
	{Name: "list_connected_browsers", Description: "List connected browsers", InputSchema: objSchema()},
	{Name: "select_browser", Description: "Select which browser session to use", InputSchema: objSchema("deviceId")},
})

func objSchema(required ...string) json.RawMessage {
	props := map[string]any{}
	for _, r := range required {
		props[r] = map[string]any{"type": "string"}
	}
	if len(required) == 2 && required[0] == "width" {
		props["width"] = map[string]any{"type": "number"}
		props["height"] = map[string]any{"type": "number"}
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	b, _ := json.Marshal(s)
	return b
}

func mustJSONTools(t []Tool) []byte {
	b, err := json.Marshal(t)
	if err != nil {
		panic(err)
	}
	return b
}
