package browsermcp

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// dispatch.go — tools/call pipeline, mirroring yrn (mod_0802.js) and its
// helpers:
//
//	Dn  strip save_to_disk from args (computer / browser_batch) → wantsSave
//	Mn  excluded-tool + browser_batch validation → "Unknown tool:" strings
//	Kn  switch_browser   (bridge only)
//	Hn  list_connected_browsers (bridge only)
//	Gn  select_browser   (bridge only)
//	$n  multi-browser interstitial (ask the user before acting)
//	qn  navigate-without-tabId front-loads tabs_context_mcp{createIfEmpty}
//	Fn  transport result → MCP {content,isError,_meta.notices}
//	yt  save_to_disk: persist image content to a 0700 temp dir
//	fe  append tab-context text + _meta.frontLoadedTabGroupId
//	Ln  thrown-error → isError result mapping (verbatim strings)
//	L   disconnected → {content:[text:onToolCallDisconnected()]}
//
// Transport call errors are modeled as typed Go errors carrying the same
// classes real throws: SocketConnectionError (XR), NoExtensionConnectedError
// (z0t), ToolCallTimeoutError (ONe, with routingAckReceived),
// ToolCallSupersededError (W), ExtensionDisconnectedMidCallError (ice),
// RequestTooLargeError (B), DisabledError (V).

// --- error classes (names match real's .name for debuggability) ---------

// SocketConnectionError mirrors XR.
type SocketConnectionError struct{ Msg string }

func (e *SocketConnectionError) Error() string { return e.Msg }

// NoExtensionConnectedError mirrors z0t.
type NoExtensionConnectedError struct{ Msg string }

func (e *NoExtensionConnectedError) Error() string { return e.Msg }

// ToolCallTimeoutError mirrors ONe; RoutingAckReceived is tri-state
// (absent / false / true) like real's routingAckReceived.
type ToolCallTimeoutError struct {
	Msg                string
	RoutingAckReceived *bool
}

func (e *ToolCallTimeoutError) Error() string { return e.Msg }

// ToolCallSupersededError mirrors W.
type ToolCallSupersededError struct{ Msg string }

func (e *ToolCallSupersededError) Error() string { return e.Msg }

// ExtensionDisconnectedMidCallError mirrors ice.
type ExtensionDisconnectedMidCallError struct{ Msg string }

func (e *ExtensionDisconnectedMidCallError) Error() string { return e.Msg }

// RequestTooLargeError mirrors B.
type RequestTooLargeError struct{ Msg string }

func (e *RequestTooLargeError) Error() string { return e.Msg }

// DisabledError mirrors V (isDisabled → "disabled: <tool> not sent").
type DisabledError struct{ Msg string }

func (e *DisabledError) Error() string { return e.Msg }

// --- tool result shape ---------------------------------------------------

// toolResult is the MCP tools/call result object.
type toolResult struct {
	Content []any          `json:"content"`
	IsError bool           `json:"isError,omitempty"`
	Meta    map[string]any `json:"_meta,omitempty"`
}

// textResult mirrors b(e,t): {content:[{type:"text",text}],isError:true}.
func textResult(text string, meta map[string]any) *toolResult {
	return &toolResult{
		Content: []any{map[string]any{"type": "text", "text": text}},
		IsError: true,
		Meta:    meta,
	}
}

// okText is a non-error text result.
func okText(text string) *toolResult {
	return &toolResult{Content: []any{map[string]any{"type": "text", "text": text}}}
}

// --- transport interface --------------------------------------------------

// toolTransport mirrors the o parameter of yrn — the thing callTool goes
// through (bridge client `_`, socket pool `I`, or single socket `ft`).
type toolTransport interface {
	ensureConnected() bool
	callTool(tool string, args map[string]any, opts *callOpts) (map[string]any, error)
	isConnected() bool
	disconnect()
	setNotificationHandler(func(method string, params json.RawMessage))
	// bridge-only capabilities:
	listConnectedExtensions() []extensionInfo
	selectExtensionByID(deviceID, name string)
	switchBrowser() (string, any) // "no_other_browsers" | nil | {deviceId,name}
	hasActiveSelection() bool
	getSelectedDeviceID() string
}

// extensionInfo mirrors the sanitized bridge extension shape (mod_0803.js U).
type extensionInfo struct {
	DeviceID       string `json:"deviceId"`
	Name           string `json:"name,omitempty"`
	OSPlatform     string `json:"osPlatform,omitempty"`
	ConnectedAt    int64  `json:"connectedAt"`
	IsLocal        bool   `json:"isLocal,omitempty"`
	OnThisComputer bool   `json:"onThisComputer,omitempty"`
	InUse          bool   `json:"inUse,omitempty"`
}

// callOpts mirrors the third arg of callTool / HUn(): permission_mode,
// allowed_domains, handle_permission_prompts, session_scope.
type callOpts struct {
	PermissionMode string
	SessionScope   map[string]any
}

// --- dispatch (yrn) -------------------------------------------------------

// dispatchCall mirrors yrn(e,t,n,r,i).
func (s *server) dispatchCall(p callParams) *toolResult {
	c := s.ctx
	name := p.Name
	var args map[string]any
	if len(p.Arguments) > 0 {
		json.Unmarshal(p.Arguments, &args)
	}
	args, wantsSave := stripSaveToDisk(name, args)

	if c.IsDisabled != nil && c.IsDisabled() {
		return textResult("Unknown tool: "+name, nil)
	}
	// real: opts (i in yrn) exists only when getRendererSessionScope(_meta)
	// resolves — the standalone server has none, so opts stays nil and the
	// transports apply their own defaults (bridge sends its
	// permissionMode; the socket omits permission_mode entirely).
	var opts *callOpts

	if v := validateCall(c, name, args); v != "" {
		return textResult(v, nil)
	}
	switch name {
	case "switch_browser":
		return s.switchBrowserTool()
	case "list_connected_browsers":
		return s.listConnectedBrowsersTool()
	case "select_browser":
		return s.selectBrowserTool(args)
	}

	sessionID := ""
	l := &frontLoad{args: args}
	res, err := s.forwardCall(name, args, opts, sessionID, l)
	if err != nil {
		return finishFrontLoad(l, mapCallError(err, name, c))
	}
	if l.frontErr != nil {
		return finishFrontLoad(l, l.frontErr)
	}
	res2 := mapResult(c, res)
	if wantsSave {
		res2 = c.saveScreenshots(res2)
	}
	return finishFrontLoad(l, res2)
}

// forwardCall is yrn's connected branch: ensureConnected → multi-browser
// interstitial → navigate front-load → Fn.
func (s *server) forwardCall(name string, args map[string]any, opts *callOpts, sessionID string, l *frontLoad) (map[string]any, error) {
	c := s.ctx
	connected := s.tr.ensureConnected()
	c.Logger.Infof("[%s] Server is connected: %v. Received tool call: %s with args: %s.",
		c.ServerName, connected, scrubForLog(name), elideJSON(args))
	if !connected {
		return nil, errDisconnected
	}
	if c.IsDisabled != nil && c.IsDisabled() {
		return nil, &DisabledError{Msg: "disabled: " + name + " not sent"}
	}
	if r := s.multiBrowserInterstitial(); r != nil {
		return nil, &interstitialResult{r}
	}
	if c.IsDisabled != nil && c.IsDisabled() {
		return nil, &DisabledError{Msg: "disabled: " + name + " not sent"}
	}
	fl := c.frontLoadTabContext(s.tr, name, args, opts, sessionID)
	l.args = fl.args
	l.tabContextJSON = fl.tabContextJSON
	l.tabGroupID = fl.tabGroupID
	l.err = fl.err
	if fl.err != nil {
		return nil, nil
	}
	return s.tr.callTool(name, l.args, opts)
}

// errDisconnected is an internal marker mapped to L() (the
// onToolCallDisconnected content result — not isError, matching real).
var errDisconnected = errors.New("browsermcp: transport not connected")

// interstitialResult carries the "pick a browser" isError result out of
// the normal return path.
type interstitialResult struct{ r *toolResult }

func (e *interstitialResult) Error() string { return "browsermcp: multi-browser interstitial" }

// mapCallError mirrors Ln(e,t,n).
func mapCallError(err error, tool string, c *Context) *toolResult {
	c.Logger.Infof("[%s] Error calling tool: %v", c.ServerName, err)
	var ir *interstitialResult
	if errors.As(err, &ir) {
		return ir.r
	}
	var de *DisabledError
	if errors.As(err, &de) {
		return textResult("Unknown tool: "+tool, nil)
	}
	var sce *SocketConnectionError
	var nee *NoExtensionConnectedError
	if errors.As(err, &sce) || errors.As(err, &nee) || errors.Is(err, errDisconnected) {
		return &toolResult{Content: []any{map[string]any{
			"type": "text", "text": c.disconnectedText()}}}
	}
	var te *ToolCallTimeoutError
	if errors.As(err, &te) {
		if te.RoutingAckReceived != nil && !*te.RoutingAckReceived {
			return textResult(fmt.Sprintf("The %q call was never delivered to the Chrome extension: the computer running that browser appears to be offline or asleep, or Chrome is not running there. This is a connection problem, not a permission or site-access issue. Ask the user to make sure that computer is awake and Chrome is open with the Claude extension, then retry.", tool),
				map[string]any{"isBridgeTimeout": true})
		}
		return textResult(timeoutMessage(tool, te), map[string]any{"isBridgeTimeout": true})
	}
	var se *ToolCallSupersededError
	if errors.As(err, &se) {
		return textResult(fmt.Sprintf("The %q tool call was displaced by a newer call on the same browser connection before its result arrived. "+
			"The action may already have executed — check the page state (e.g. \"get_page_text\") before repeating it.", tool), nil)
	}
	var ee *ExtensionDisconnectedMidCallError
	if errors.As(err, &ee) {
		return textResult(fmt.Sprintf("The %q tool call failed because the Chrome extension disconnected mid-operation. This is usually transient (Chrome service worker restart, tab closed, network blip) and the extension often reconnects automatically. Retry the same tool call in a few seconds. If it keeps failing, ask the user to switch to Chrome (which wakes the extension) or check that the extension is still logged in.", tool), nil)
	}
	var tl *RequestTooLargeError
	if errors.As(err, &tl) {
		return textResult(fmt.Sprintf("The %q tool call was not sent: %s Do not retry the identical call.", tool, tl.Msg), nil)
	}
	return textResult(fmt.Sprintf("The %q tool call failed unexpectedly. This is usually transient — retry the same call in a few seconds.", tool), nil)
}

// timeoutMessage mirrors jn(e,t).
func timeoutMessage(tool string, te *ToolCallTimeoutError) string {
	head := fmt.Sprintf("The %q tool did not respond in time.", tool)
	if te.RoutingAckReceived == nil {
		return head + " The page or Chrome may be unresponsive, or Chrome may be waiting for the user. Try a lighter operation (e.g., \"get_page_text\" instead of a screenshot), or ask the user to check Chrome."
	}
	return head + " If Chrome is on another computer, that computer may be closed or asleep. The page may also be loading or unresponsive, or Chrome may be waiting for the user. Try a lighter operation (e.g., \"get_page_text\" instead of a screenshot) once. If that also gets no response, ask the user to check that the computer running Chrome is awake, that Chrome is open, and that nothing in Chrome is waiting for them."
}

// disconnectedText mirrors L(e).
func (c *Context) disconnectedText() string {
	if c.OnToolCallDisconnected != nil {
		return c.OnToolCallDisconnected()
	}
	return "Browser extension is not connected."
}

// --- arg pre-processing (Dn / Mn) ----------------------------------------

// stripSaveToDisk mirrors Dn(e,t): removes save_to_disk; reports whether
// any true value was seen (computer or batch action input).
func stripSaveToDisk(tool string, args map[string]any) (map[string]any, bool) {
	if args == nil {
		return nil, false
	}
	if tool == "browser_batch" {
		actions, ok := args["actions"].([]any)
		if !ok {
			return args, false
		}
		wants := false
		out := map[string]any{}
		for k, v := range args {
			if k != "save_to_disk" {
				out[k] = v
			}
		}
		if sd, _ := args["save_to_disk"].(bool); sd {
			wants = true
		}
		newActions := make([]any, len(actions))
		for i, a := range actions {
			am, ok := a.(map[string]any)
			if !ok {
				newActions[i] = a
				continue
			}
			in, ok := am["input"].(map[string]any)
			if !ok {
				newActions[i] = a
				continue
			}
			na := map[string]any{}
			for k, v := range am {
				na[k] = v
			}
			ni := map[string]any{}
			for k, v := range in {
				ni[k] = v
			}
			if sd, _ := ni["save_to_disk"].(bool); sd {
				if n, _ := na["name"].(string); n == "computer" {
					wants = true
				}
			}
			delete(ni, "save_to_disk")
			na["input"] = ni
			newActions[i] = na
		}
		out["actions"] = newActions
		return out, wants
	}
	if _, has := args["save_to_disk"]; !has {
		return args, false
	}
	wants := false
	if sd, _ := args["save_to_disk"].(bool); sd && tool == "computer" {
		wants = true
	}
	out := map[string]any{}
	for k, v := range args {
		if k != "save_to_disk" {
			out[k] = v
		}
	}
	return out, wants
}

// validateCall mirrors Mn(e,t,n): excluded tools → "Unknown tool:",
// browser_batch array/nesting/excluded checks.
func validateCall(c *Context, tool string, args map[string]any) string {
	if len(c.ExcludedTools) == 0 {
		return ""
	}
	if c.ExcludedTools[tool] {
		return "Unknown tool: " + tool
	}
	if tool != "browser_batch" {
		return ""
	}
	actions, ok := args["actions"].([]any)
	if !ok {
		return "browser_batch: actions must be an array"
	}
	for _, a := range actions {
		am, _ := a.(map[string]any)
		n, _ := am["name"].(string)
		if n == "" || n == "browser_batch" {
			return "browser_batch: every action needs a string tool name (and batches don't nest)"
		}
		if c.ExcludedTools[n] {
			return "Unknown tool: " + n
		}
	}
	return ""
}

// --- front-loaded tab context (qn / fe) ------------------------------------

// frontLoad carries the qn() result plus what fe() appends.
type frontLoad struct {
	args           map[string]any
	tabContextJSON string
	tabGroupID     *float64
	err            *toolResult
	frontErr       *toolResult
}

var frontLoadTimeout = 8 * time.Second // kt in mod_0802.js

var frontLoadCache sync.Map // transport → *sync.Map (session → promise)

// frontLoadTabContext mirrors qn: only for `navigate` with a missing/null
// tabId and a url that isn't "back"/"forward". Without a session scope our
// standalone server can't key the lookup, so it passes args through
// unchanged (real requires sessionScope.sessionId to front-load).
func (c *Context) frontLoadTabContext(tr toolTransport, tool string, args map[string]any, opts *callOpts, sessionID string) *frontLoad {
	fl := &frontLoad{args: args}
	if tool != "navigate" {
		return fl
	}
	if _, ok := args["tabId"]; ok && args["tabId"] != nil {
		return fl
	}
	urlStr, _ := args["url"].(string)
	lc := strings.ToLower(strings.TrimSpace(urlStr))
	if lc == "back" || lc == "forward" {
		fl.err = textResult(`tabId is required for url:"back"/"forward". Call tabs_context_mcp first to get a tab ID.`, nil)
		return fl
	}
	if urlStr == "" || sessionID == "" {
		return fl
	}
	// per-transport per-session memoized tabs_context_mcp lookup
	inner, _ := frontLoadCache.LoadOrStore(tr, &sync.Map{})
	sm := inner.(*sync.Map)
	type tcResult struct {
		res map[string]any
		err error
	}
	ch := make(chan tcResult, 1)
	if _, loaded := sm.LoadOrStore(sessionID, ch); !loaded {
		go func() {
			var o callOpts
			if opts != nil {
				o = *opts
			}
			o.PermissionMode = "ask"
			res, err := tr.callTool("tabs_context_mcp", map[string]any{"createIfEmpty": true}, &o)
			ch <- tcResult{res, err}
			sm.Delete(sessionID)
		}()
	}
	select {
	case r := <-ch:
		if r.err != nil {
			content := []any{map[string]any{"type": "text", "text": "tabs_context_mcp returned an error."}}
			if ec, ok := r.res["error"].(map[string]any); ok {
				if cl, ok := ec["content"].([]any); ok {
					content = cl
				}
			}
			if containsReauth(content) && c.OnAuthenticationError != nil {
				c.OnAuthenticationError()
			}
			fl.err = &toolResult{Content: content, IsError: true}
			return fl
		}
		tabID, groupID, raw := extractTabContext(r.res)
		fl.tabGroupID = groupID
		if tabID == nil {
			return fl
		}
		args2 := map[string]any{}
		for k, v := range args {
			args2[k] = v
		}
		args2["tabId"] = *tabID
		fl.args = args2
		fl.tabContextJSON = raw
		return fl
	case <-time.After(frontLoadTimeout):
		fl.err = textResult(fmt.Sprintf("The hidden tabs_context_mcp lookup did not respond within %ds. The Chrome extension may be slow to start, or the computer running that browser may be offline or asleep. Retry navigate, or call tabs_context_mcp explicitly to get a tabId first.", int(frontLoadTimeout/time.Second)),
			map[string]any{"isFrontLoadBoundExceeded": true})
		return fl
	}
}

// extractTabContext mirrors V0t({result}): find the text item whose JSON
// has availableTabs / tabGroupId.
func extractTabContext(res map[string]any) (tabID *float64, groupID *float64, raw string) {
	result, _ := res["result"].(map[string]any)
	content, _ := result["content"].([]any)
	for _, it := range content {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] != "text" {
			continue
		}
		txt, _ := m["text"].(string)
		var parsed map[string]any
		if json.Unmarshal([]byte(txt), &parsed) != nil {
			continue
		}
		tabs, ok := parsed["availableTabs"].([]any)
		if !ok {
			continue
		}
		for _, t := range tabs {
			if tm, ok := t.(map[string]any); ok {
				if id, ok := tm["tabId"].(float64); ok {
					tabID = &id
					break
				}
			}
		}
		if g, ok := parsed["tabGroupId"].(float64); ok {
			groupID = &g
		}
		return tabID, groupID, txt
	}
	return nil, nil, ""
}

// finishFrontLoad mirrors fe(e,t): append the tab-context text row and
// _meta.frontLoadedTabGroupId.
func finishFrontLoad(l *frontLoad, r *toolResult) *toolResult {
	if l.err != nil {
		r = l.err
	}
	if l.tabContextJSON != "" && !r.IsError {
		r.Content = append(r.Content, map[string]any{
			"type": "text",
			"text": "\nTab context (from front-loaded tabs_context_mcp):\n" + l.tabContextJSON +
				"\nTabs in this group were opened for this task and are yours to clean up: close each with tabs_close_mcp once you no longer need it and before finishing, unless the user asked to see it or wants it kept open.",
		})
	}
	if l.tabGroupID != nil {
		if r.Meta == nil {
			r.Meta = map[string]any{}
		}
		r.Meta["frontLoadedTabGroupId"] = *l.tabGroupID
	}
	return r
}

// --- result mapping (Fn / Pt / yt) -----------------------------------------

// mapResult mirrors Fn(e,t,n,r,i): {result|error, notices} → MCP shape.
func mapResult(c *Context, res map[string]any) *toolResult {
	if res == nil {
		return okText("Tool execution completed")
	}
	c.Logger.Infof("[%s] Received result from socket bridge: %s", c.ServerName, elideJSON(res))
	result, _ := res["result"].(map[string]any)
	errObj, hasErr := res["error"].(map[string]any)
	notices, _ := res["notices"].([]any)
	var meta map[string]any
	if len(notices) > 0 {
		meta = map[string]any{"notices": notices}
	}
	l := result
	if hasErr {
		l = errObj
	}
	if l == nil {
		return &toolResult{
			Content: []any{map[string]any{"type": "text", "text": "Tool execution completed"}},
			Meta:    meta,
		}
	}
	content := l["content"]
	if hasErr {
		if cl, ok := content.([]any); ok && containsReauth(cl) && c.OnAuthenticationError != nil {
			c.OnAuthenticationError()
		}
	}
	switch cl := content.(type) {
	case []any:
		out := make([]any, 0, len(cl))
		for _, d := range cl {
			m, ok := d.(map[string]any)
			if !ok {
				out = append(out, map[string]any{"type": "text", "text": fmt.Sprint(d)})
				continue
			}
			// image source flattening: {type:"image", source:{data, media_type}}
			if m["type"] == "image" && !hasErr {
				if src, ok := m["source"].(map[string]any); ok {
					if data, ok := src["data"].(string); ok {
						mt := "image/png"
						if v, ok := src["media_type"].(string); ok && v != "" {
							mt = v
						}
						out = append(out, map[string]any{"type": "image", "data": data, "mimeType": mt})
						continue
					}
				}
			}
			if _, hasType := m["type"]; hasType {
				out = append(out, m)
			} else {
				out = append(out, map[string]any{"type": "text", "text": fmt.Sprint(d)})
			}
		}
		return &toolResult{Content: out, IsError: hasErr, Meta: meta}
	case string:
		return &toolResult{
			Content: []any{map[string]any{"type": "text", "text": cl}},
			IsError: hasErr, Meta: meta,
		}
	default:
		c.Logger.Warnf("[%s] Unexpected result format from socket bridge", c.ServerName)
		b, _ := json.Marshal(res)
		return &toolResult{
			Content: []any{map[string]any{"type": "text", "text": string(b)}},
			IsError: hasErr, Meta: meta,
		}
	}
}

// containsReauth mirrors Pt(e) — content whose joined text mentions
// "re-authenticated" triggers onAuthenticationError.
func containsReauth(content []any) bool {
	return strings.Contains(strings.ToLower(joinContentText(content)), "re-authenticated")
}

// joinContentText mirrors he(e).
func joinContentText(content []any) string {
	var parts []string
	for _, it := range content {
		switch v := it.(type) {
		case string:
			parts = append(parts, v)
		case map[string]any:
			if t, ok := v["text"].(string); ok {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, " ")
}

// elideJSON mirrors Tt(e): stringify with >256-char strings elided.
func elideJSON(v any) string {
	b, _ := json.Marshal(v)
	// cheap version of the replacer: post-process long strings
	var walk func(x any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case string:
			if len(t) > 256 {
				return fmt.Sprintf("<%d chars elided>", len(t))
			}
			return t
		case map[string]any:
			m := map[string]any{}
			for k, v := range t {
				m[k] = walk(v)
			}
			return m
		case []any:
			a := make([]any, len(t))
			for i, v := range t {
				a[i] = walk(v)
			}
			return a
		default:
			return x
		}
	}
	b, _ = json.Marshal(walk(v))
	return string(b)
}

// --- save_to_disk (yt / gt / In) ---------------------------------------------

var (
	shotDirOnce sync.Once
	shotDir     string
	shotDirErr  error
	shotSeq     int
)

var imageExt = map[string]string{ // ht in mod_0802.js
	"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp", "image/gif": "gif",
}

// screenshotDir mirrors gt()/In(): mkdtemp(os.tmpdir, "claude-chrome-screenshots-"),
// reused only if the same uid owns it at mode 0700.
func (c *Context) screenshotDir() (string, error) {
	if c.ScreenshotsDir != nil {
		d := c.ScreenshotsDir()
		if d == "" {
			return "", nil // save_to_disk has no effect (remote no-dir case)
		}
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", err
		}
		return d, nil
	}
	shotDirOnce.Do(func() {
		shotDir, shotDirErr = os.MkdirTemp("", "claude-chrome-screenshots-")
		if shotDirErr == nil {
			_ = os.Chmod(shotDir, 0o700)
		}
	})
	if shotDirErr != nil {
		return "", shotDirErr
	}
	// reuse guard (In): dir must be ours and exactly 0700
	fi, err := os.Stat(shotDir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		d, err := os.MkdirTemp("", "claude-chrome-screenshots-")
		if err != nil {
			return "", err
		}
		_ = os.Chmod(d, 0o700)
		shotDir = d
	}
	return shotDir, nil
}

// saveScreenshots mirrors yt(e,t): writes image content to disk as
// screenshot-<ms>-<n>.<ext> mode 0600 (wx) and appends guidance text.
func (c *Context) saveScreenshots(r *toolResult) *toolResult {
	hasImage := false
	for _, it := range r.Content {
		if m, ok := it.(map[string]any); ok && m["type"] == "image" {
			hasImage = true
		}
	}
	if r.IsError || !hasImage {
		return r
	}
	dir, err := c.screenshotDir()
	if err != nil {
		c.Logger.Warnf("[%s] save_to_disk: could not prepare screenshot directory: %v", c.ServerName, err)
		r.Content = append(r.Content, map[string]any{"type": "text",
			"text": "Note: save_to_disk failed — the screenshot directory could not be created. The image is included inline above."})
		return r
	}
	if dir == "" {
		r.Content = append(r.Content, map[string]any{"type": "text",
			"text": "Note: save_to_disk had no effect — screenshots are not persisted to disk in this session. The image is included inline above; refer to it directly. Do not retry with save_to_disk."})
		return r
	}
	var out []any
	saved, failed := false, false
	for _, it := range r.Content {
		out = append(out, it)
		m, ok := it.(map[string]any)
		if !ok || m["type"] != "image" {
			continue
		}
		mt, _ := m["mimeType"].(string)
		ext, ok := imageExt[mt]
		if !ok {
			ext = "png"
		}
		shotSeq++
		p := filepath.Join(dir, fmt.Sprintf("screenshot-%d-%d.%s", time.Now().UnixMilli(), shotSeq-1, ext))
		data, _ := m["data"].(string)
		raw, derr := base64.StdEncoding.DecodeString(data)
		if derr != nil {
			failed = true
			continue
		}
		werr := writeFileExclusive(p, raw, 0o600)
		if werr != nil {
			failed = true
			c.Logger.Warnf("[%s] save_to_disk: failed to write screenshot: %v", c.ServerName, werr)
			continue
		}
		saved = true
		out = append(out, map[string]any{"type": "text", "text": "Screenshot saved to: " + p})
	}
	if saved {
		out = append(out, map[string]any{"type": "text",
			"text": "Include the saved path(s) in your response so they can be attached for the user."})
	}
	if failed {
		out = append(out, map[string]any{"type": "text",
			"text": "Note: save_to_disk failed for at least one screenshot — it could not be written to disk. The image is included inline above."})
	}
	r.Content = out
	return r
}

// writeFileExclusive mirrors Sn(...,{flag:"wx"}).
func writeFileExclusive(p string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

// --- multi-browser interstitial + browser picker tools (Kn/Hn/Gn/$n) --------

// switchBrowserTool mirrors Kn(e,t): `t.switchBrowser?.() ?? null`.
func (s *server) switchBrowserTool() *toolResult {
	c := s.ctx
	if c.BridgeConfig == nil {
		return textResult("Browser switching is only available with bridge connections.", nil)
	}
	if !s.tr.ensureConnected() {
		return s.disconnectedResult()
	}
	if c.IsDisabled != nil && c.IsDisabled() {
		return textResult("Unknown tool: switch_browser", nil)
	}
	status, v := s.tr.switchBrowser()
	if status == "no_other_browsers" {
		return &toolResult{Content: []any{map[string]any{"type": "text",
			"text": "No other browsers available to switch to. Open Chrome with the Claude extension in another browser to switch."}},
			IsError: false}
	}
	if m, ok := v.(map[string]string); ok && m != nil {
		return okText(fmt.Sprintf("Selected browser %q.", displayName(m["name"], 0)))
	}
	return textResult("No browser responded within the timeout. Make sure Chrome is open with the Claude extension installed, then try again.", nil)
}

// listConnectedBrowsersTool mirrors Hn(e,t): real gates on
// `!e.bridgeConfig || !t.listConnectedExtensions` — i.e. bridge transport
// only, by capability presence not call result.
func (s *server) listConnectedBrowsersTool() *toolResult {
	c := s.ctx
	if _, isBridge := s.tr.(*bridgeTransport); c.BridgeConfig == nil || !isBridge {
		return textResult("Listing browsers is only available with bridge connections.", nil)
	}
	if !s.tr.ensureConnected() {
		return s.disconnectedResult()
	}
	if c.IsDisabled != nil && c.IsDisabled() {
		return textResult("Unknown tool: list_connected_browsers", nil)
	}
	exts := s.tr.listConnectedExtensions()
	var local map[string]string
	if len(exts) > 1 && c.LocalDeviceIDs != nil {
		local = c.LocalDeviceIDs()
	}
	pick := pickExtension(exts, s.tr.getSelectedDeviceID(), local)
	selected := ""
	if pick.kind == "select" {
		selected = pick.extension.DeviceID
	}
	out := make([]extensionInfo, len(exts))
	for i, e := range exts {
		e.Name = rawDisplayName(e.Name, i)
		if e.OSPlatform != "" {
			if v, ok := osPlatformMap[e.OSPlatform]; ok {
				e.OSPlatform = v
			}
		}
		if e.DeviceID == selected {
			e.InUse = true
		}
		out[i] = e
	}
	b, _ := json.Marshal(out)
	content := []any{map[string]any{"type": "text", "text": string(b)}}
	if len(exts) > 1 {
		content = append(content, map[string]any{"type": "text",
			"text": s.browserSummaryText(out, pick)})
	}
	return &toolResult{Content: content}
}

// browserSummaryText mirrors Wn(e,t,n,r).
func (s *server) browserSummaryText(n []extensionInfo, pick hrnResult) string {
	if pick.kind == "select" {
		verb := "is the one on this computer and will be used"
		if pick.reason == "persisted" {
			if s.tr.hasActiveSelection() {
				verb = "is the one in use for this session"
			} else {
				verb = "is the one picked last and will be used"
			}
		}
		idx := -1
		for i, e := range n {
			if e.DeviceID == pick.extension.DeviceID {
				idx = i
			}
		}
		return fmt.Sprintf("%d browsers are connected; %q (deviceId %s) %s. No question is needed unless the user wants a different browser (then select_browser with its deviceId, or switch_browser).",
			len(n), displayName(pick.extension.Name, idx), pick.extension.DeviceID, verb)
	}
	if pick.kind == "ask" && pick.localCount > 0 {
		return fmt.Sprintf("%d browsers are connected, %d of them on this computer (onThisComputer: true), and none is selected for this session. %s List the browsers on this computer first.",
			len(n), pick.localCount, pickToolQuestion(s.ctx.AskUserToolName))
	}
	return fmt.Sprintf("%d browsers are connected and none is selected for this session. %s",
		len(n), pickToolQuestion(s.ctx.AskUserToolName))
}

// selectBrowserTool mirrors Gn(e,t,n) — `!e.bridgeConfig ||
// !t.selectExtensionById || !t.listConnectedExtensions || !r`.
func (s *server) selectBrowserTool(args map[string]any) *toolResult {
	c := s.ctx
	deviceID, _ := args["deviceId"].(string)
	if _, isBridge := s.tr.(*bridgeTransport); c.BridgeConfig == nil || !isBridge || deviceID == "" {
		return textResult("select_browser requires a bridge connection and a deviceId argument.", nil)
	}
	if !s.tr.ensureConnected() {
		return s.disconnectedResult()
	}
	exts := s.tr.listConnectedExtensions()
	var found *extensionInfo
	var idx int
	for i, e := range exts {
		if e.DeviceID == deviceID {
			cp := e
			found = &cp
			idx = i
		}
	}
	if found == nil {
		return textResult(fmt.Sprintf("No connected browser has deviceId %q. Call list_connected_browsers to see currently connected browsers.", deviceID), nil)
	}
	if c.IsDisabled != nil && c.IsDisabled() {
		return textResult("Unknown tool: select_browser", nil)
	}
	s.tr.selectExtensionByID(deviceID, rawDisplayName(found.Name, idx))
	return okText(fmt.Sprintf("Selected browser %q.", displayName(found.Name, idx)))
}

// disconnectedResult mirrors L(e) — note: NOT isError (real returns
// {content:[...]} with no isError).
func (s *server) disconnectedResult() *toolResult {
	return &toolResult{Content: []any{map[string]any{
		"type": "text", "text": s.ctx.disconnectedText()}}}
}

// multiBrowserInterstitial mirrors $n(e,t): when several browsers are
// connected and none selected → isError result instructing the model to
// ask the user. Socket transports return nil once checked.
func (s *server) multiBrowserInterstitial() *toolResult {
	c := s.ctx
	tr := s.tr
	// $n: socket clients (no listConnectedExtensions support) are marked
	// checked once and pass through.
	bt, isBridge := tr.(*bridgeTransport)
	if !isBridge {
		return nil
	}
	if bt.hasActiveSelection() {
		return nil
	}
	exts := tr.listConnectedExtensions()
	if len(exts) == 0 {
		return nil
	}
	var local map[string]string
	if len(exts) > 1 && c.LocalDeviceIDs != nil {
		local = c.LocalDeviceIDs()
	}
	pick := pickExtension(exts, tr.getSelectedDeviceID(), local)
	if pick.kind != "ask" {
		return nil
	}
	shown := pick.candidates
	if len(shown) > 8 {
		shown = shown[:8]
	}
	line := func(e extensionInfo, i int) string {
		tag := ""
		if local != nil && local[e.DeviceID] == "remembered" {
			tag = " — seen recently, not answering right now"
		}
		return fmt.Sprintf("%d. %q (%s)%s — deviceId: %s",
			i+1, displayName(e.Name, i), mapOSPlatform(e.OSPlatform), tag, e.DeviceID)
	}
	var localLines, otherLines []string
	for i, e := range shown {
		if i < pick.localCount {
			localLines = append(localLines, line(e, i))
		} else {
			otherLines = append(otherLines, line(e, i))
		}
	}
	var listing string
	if pick.localCount > 0 {
		l := "Browsers on this computer:\n" + strings.Join(localLines, "\n") + "\n\nOther connected browsers:\n"
		if len(otherLines) > 0 {
			l += strings.Join(otherLines, "\n")
		} else if len(shown) == pick.localCount {
			l += "(none)"
		}
		listing = l
	} else {
		listing = "Connected browsers:\n" + strings.Join(otherLines, "\n")
	}
	more := ""
	if len(pick.candidates) > len(shown) {
		more = fmt.Sprintf("\n…and %d more not shown", len(pick.candidates)-len(shown))
	}
	head := "Multiple Chrome browsers are connected to this account and none has been selected for this session. "
	if pick.localCount > 0 {
		head = fmt.Sprintf("%d Chrome browsers are connected to this account, %d of them on this computer, and none has been selected for this session. ", len(pick.candidates), pick.localCount)
	}
	suffix := ""
	if pick.localCount > 0 {
		suffix = " List the browsers on this computer first, in the order below."
	}
	return textResult(head+pickToolQuestion(c.AskUserToolName)+suffix+"\n\n"+listing+more, nil)
}

// displayName mirrors z(e,t): sanitize + "Browser N" fallback + dedupe.
func displayName(name string, idx int) string {
	s := rawDisplayName(name, idx)
	return s
}

// rawDisplayName mirrors ge(e,t): clean control chars, cap 50, fallback.
func rawDisplayName(name string, idx int) string {
	n := sanitizeString(name, 50)
	n = trimSpace(n)
	if n == "" {
		return fmt.Sprintf("Browser %d", idx+1)
	}
	return n
}

// osPlatformMap mirrors xt in mod_0802.js.
var osPlatformMap = map[string]string{
	"macOS": "macOS", "Windows": "Windows", "Linux": "Linux",
	"Chrome OS": "Chrome OS", "Chromium OS": "Chromium OS",
	"Android": "Android", "iOS": "iOS",
	"MacIntel": "macOS", "Win32": "Windows",
	"Linux x86_64": "Linux", "Linux aarch64": "Linux", "Linux armv8l": "Linux",
}

func mapOSPlatform(p string) string {
	if p == "" {
		return "unknown OS"
	}
	if v, ok := osPlatformMap[p]; ok {
		return v
	}
	return "unknown OS"
}
