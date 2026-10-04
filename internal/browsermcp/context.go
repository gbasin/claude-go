package browsermcp

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// small indirections so tests can swap them
var (
	jsonUnmarshal     = json.Unmarshal
	jsonMarshalIndent = func(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }
)

// OAuthSource supplies claude.ai OAuth tokens + account identity for the
// bridge dial (bridgeConfig.getOAuthToken/getUserId in real).
type OAuthSource interface {
	// Token returns the current OAuth access token, refreshing first;
	// "" when signed out / API-key auth.
	Token() string
	// BearerAllowed reports whether the org policy gate permits handing
	// this bearer to the bridge (mod_0807.js Y() bearerGate).
	BearerAllowed(token string) bool
}

// bugReportURL is mod_0807.js M.
const bugReportURL = "https://github.com/anthropics/claude-code/issues/new?labels=bug,claude-in-chrome"

// installURL is mod_0278.js kH.
const installURL = "https://claude.ai/chrome"

// Context mirrors mod_0807.js K() / createChromeContext — everything the
// MCP server and its transports need:
//
//	serverName, logger, socketPath, getSocketPaths, clientTypeId,
//	onAuthenticationError, onToolCallDisconnected, onExtensionPaired,
//	getPersistedDeviceId, getLocalDeviceIds, getHostPlatform,
//	askUserToolName, getScreenshotSaveDir, bridgeConfig,
//	initialPermissionMode, trackEvent.
//
// Env vars honored (real):
//
//	CLAUDE_CODE_REMOTE_SESSION_ID   marks remote sessions (screenshots dir)
//	CLAUDE_CHROME_PERMISSION_MODE   ask|skip_all_permission_checks|follow_a_plan
//	CLAUDE_CHROME_PAIRED_DEVICE_ID  [A-Za-z0-9_-]{1,128}
//	CLAUDE_CHROME_TAB_GROUP_KEY     "chat:" + lowercase UUID
//	USE_LOCAL_OAUTH / LOCAL_BRIDGE  bridge URL = ws://localhost:8765
//	USE_STAGING_OAUTH               bridge URL = wss://bridge-staging.claudeusercontent.com
//	CLAUDE_CODE_ACCOUNT_UUID        userId fallback for the bridge dial
type Context struct {
	ServerName   string
	Logger       Logger
	ClientTypeID string

	// SocketPath is this process's native-host socket (mod_0233.js OBt);
	// SocketPaths scans the dir for all live ones (mod_0233.js A6r).
	SocketPath  string
	SocketPaths func() []string

	// BridgeConfig, when non-nil, selects the WebSocket bridge transport
	// (mod_0803.js BAo). Run() always sets it, like real.
	BridgeConfig *BridgeConfig

	// InitialPermissionMode from CLAUDE_CHROME_PERMISSION_MODE.
	InitialPermissionMode string
	// PairedDeviceIDEnv is CLAUDE_CHROME_PAIRED_DEVICE_ID (validated).
	PairedDeviceIDEnv string

	// AskUserToolName mirrors context.askUserToolName (zs —
	// "AskUserQuestion"). "" when the host has no such tool.
	AskUserToolName string

	// ExcludedTools mirrors context.excludedTools (dropped from tools/list
	// and refused in tools/call with "Unknown tool:").
	ExcludedTools map[string]bool

	// HostTools mirrors context.hostTools() — extra entries appended to
	// tools/list. Nil in the standalone server.
	HostTools func() []Tool

	// IsDisabled mirrors context.isDisabled — when true tools/list is
	// empty and calls return "Unknown tool:" errors.
	IsDisabled func() bool

	// ScreenshotsDir mirrors getScreenshotSaveDir — set only for remote
	// sessions (CLAUDE_CODE_REMOTE_SESSION_ID present); "" → mkdtemp.
	ScreenshotsDir func() string

	// Hooks — all optional.
	OnAuthenticationError  func()
	OnToolCallDisconnected func() string
	OnExtensionPaired      func(deviceID, name string)
	PersistedDeviceID      func() string
	LocalDeviceIDs         func() map[string]string // deviceId -> "live"|"remembered"
	HostPlatform           func() string
}

// Logger is the small leveled logger real wraps around its debug sink
// (mod_0807.js class N). Lines go to stderr — stdout is the JSON-RPC
// channel and must stay clean.
type Logger interface {
	Debugf(format string, args ...any)
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// StdLogger writes `[Claude in Chrome] ...` to stderr.
type StdLogger struct{}

func (StdLogger) Debugf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "[Claude in Chrome] "+f+"\n", a...)
}
func (StdLogger) Infof(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "[Claude in Chrome] "+f+"\n", a...)
}
func (StdLogger) Warnf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "[Claude in Chrome] "+f+"\n", a...)
}
func (StdLogger) Errorf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "[Claude in Chrome] "+f+"\n", a...)
}

// permissionModes is mod_0807.js x — valid CLAUDE_CHROME_PERMISSION_MODEs.
var permissionModes = []string{"ask", "skip_all_permission_checks", "follow_a_plan"}

// pairedDeviceIDRe is mod_0807.js H — validates CLAUDE_CHROME_PAIRED_DEVICE_ID.
var pairedDeviceIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// tabGroupKeyRe validates CLAUDE_CHROME_TAB_GROUP_KEY (mod_0807.js Skr):
// "chat:" + lowercase UUID.
var tabGroupKeyRe = regexp.MustCompile(`^chat:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// BridgeURL mirrors mod_0807.js G().
func BridgeURL(env func(string) string) string {
	if env("USE_LOCAL_OAUTH") != "" || env("LOCAL_BRIDGE") != "" {
		return "ws://localhost:8765"
	}
	if env("USE_STAGING_OAUTH") != "" {
		return "wss://bridge-staging.claudeusercontent.com"
	}
	return "wss://bridge.claudeusercontent.com"
}

// IsLocalBridge mirrors mod_0807.js q().
func IsLocalBridge(env func(string) string) bool {
	return env("USE_LOCAL_OAUTH") != "" || env("LOCAL_BRIDGE") != ""
}

// socketDir is mod_0233.js IBt: /tmp/claude-mcp-browser-bridge-<user>.
func socketDir() string {
	return filepath.Join(os.TempDir(), "claude-mcp-browser-bridge-"+socketUser())
}

// socketUser is mod_0233.js T: os.userInfo().username, USER/USERNAME
// fallback, "default" last.
func socketUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		// os.userInfo().username may carry DOMAIN\user on Windows.
		if i := strings.LastIndexByte(u.Username, '\\'); i >= 0 {
			return u.Username[i+1:]
		}
		return u.Username
	}
	for _, e := range []string{"USER", "USERNAME"} {
		if v := os.Getenv(e); v != "" {
			return v
		}
	}
	return "default"
}

// SocketPath mirrors mod_0233.js OBt: win32 → \\.\pipe\<name>;
// elsewhere → <dir>/<pid>.sock.
func SocketPath() string {
	if runtime.GOOS == "windows" {
		return `\\.\pipe\claude-mcp-browser-bridge-` + socketUser()
	}
	return filepath.Join(socketDir(), fmt.Sprintf("%d.sock", os.Getpid()))
}

// SocketPaths mirrors mod_0233.js A6r: all *.sock in the bridge dir
// (win32 → the single pipe path).
func SocketPaths() []string {
	if runtime.GOOS == "windows" {
		return []string{`\\.\pipe\claude-mcp-browser-bridge-` + socketUser()}
	}
	ents, err := os.ReadDir(socketDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".sock") {
			out = append(out, filepath.Join(socketDir(), e.Name()))
		}
	}
	return out
}

// NewContext mirrors the K() call inside runClaudeInChromeMcpServer
// (mod_0807.js): all fields the stdio server path populates. oauth is the
// credential supplier used by the bridge dial; persistPairing writes
// chromeExtension.{pairedDeviceId,pairedDeviceName} to ~/.claude.json.
func NewContext(log Logger, oauth OAuthSource, persistPairing func(deviceID, name string)) *Context {
	env := os.Getenv
	c := &Context{
		ServerName:    ServerDisplayName,
		Logger:        log,
		ClientTypeID:  "claude-code",
		SocketPath:    SocketPath(),
		SocketPaths:   SocketPaths,
		ExcludedTools: map[string]bool{},
		HostTools:     func() []Tool { return nil },
	}
	if m := env("CLAUDE_CHROME_PERMISSION_MODE"); m != "" {
		ok := false
		for _, v := range permissionModes {
			if v == m {
				ok = true
			}
		}
		if ok {
			c.InitialPermissionMode = m
		} else {
			log.Warnf(`Invalid CLAUDE_CHROME_PERMISSION_MODE %q. Valid values: %s`, m, strings.Join(permissionModes, ", "))
		}
	}
	if d := env("CLAUDE_CHROME_PAIRED_DEVICE_ID"); d != "" {
		if pairedDeviceIDRe.MatchString(d) {
			c.PairedDeviceIDEnv = d
		} else {
			log.Warnf("Ignoring CLAUDE_CHROME_PAIRED_DEVICE_ID: not a browser extension device id")
		}
	}
	switch tabGroupKeyState(env("CLAUDE_CHROME_TAB_GROUP_KEY")) {
	case "malformed":
		log.Warnf(`Ignoring CLAUDE_CHROME_TAB_GROUP_KEY: expected "chat:" followed by a lowercase UUID.`)
	case "ignored":
		log.Infof("Ignoring CLAUDE_CHROME_TAB_GROUP_KEY: this process hosts several sessions, so each uses its own tab group.")
	case "used":
		log.Infof("CLAUDE_CHROME_TAB_GROUP_KEY is set and well-formed. Claude Code's own browser calls carry it as the tab group key unless this process hosts several sessions.")
	}
	c.BridgeConfig = &BridgeConfig{
		URL:         BridgeURL(env),
		DevUserID:   localDevUser(env),
		OAuth:       oauth,
		AccountUUID: accountUUIDHint(),
	}
	c.OnAuthenticationError = func() {
		log.Warnf("Authentication error occurred. Please ensure you are logged into the Claude browser extension with the same claude.ai account as Claude Code.")
	}
	c.OnToolCallDisconnected = c.disconnectedMessage
	c.OnExtensionPaired = func(deviceID, name string) {
		if persistPairing != nil {
			persistPairing(deviceID, name)
		}
		short := deviceID
		if len(short) > 8 {
			short = short[:8]
		}
		log.Infof(`Paired with %q (%s)`, name, short)
	}
	c.PersistedDeviceID = func() string {
		if v := persistedDeviceIDFromConfig(); v != "" {
			return v
		}
		return c.PairedDeviceIDEnv
	}
	c.LocalDeviceIDs = func() map[string]string { return nil } // browserHints state file is daemon-internal
	c.HostPlatform = func() string { return runtime.GOOS }
	if env("CLAUDE_CODE_REMOTE_SESSION_ID") != "" {
		// real: remote sessions get a fixed screenshot dir (O!==void 0)
		c.ScreenshotsDir = func() string { return "" }
	}
	return c
}

// localDevUser mirrors `...q()&&{devUserId:"dev_user_local"}`.
func localDevUser(env func(string) string) string {
	if IsLocalBridge(env) {
		return "dev_user_local"
	}
	return ""
}

func accountUUIDHint() string {
	if v := os.Getenv("CLAUDE_CODE_ACCOUNT_UUID"); v != "" {
		return v
	}
	return oauthAccountUUID()
}

// tabGroupKeyState mirrors Skr().
func tabGroupKeyState(v string) string {
	if v == "" {
		return "unset"
	}
	if !tabGroupKeyRe.MatchString(v) {
		return "malformed"
	}
	// "ignored" applies when this process hosts several sessions — the
	// standalone MCP server hosts exactly one, so well-formed → "used".
	return "used"
}

// oauthAccountUUID reads oauthAccount.accountUuid from ~/.claude.json
// (le().oauthAccount?.accountUuid).
func oauthAccountUUID() string {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return ""
	}
	var m map[string]any
	if jsonUnmarshal(b, &m) != nil {
		return ""
	}
	if oa, ok := m["oauthAccount"].(map[string]any); ok {
		if v, _ := oa["accountUuid"].(string); v != "" {
			return v
		}
	}
	return ""
}

// persistedDeviceIDFromConfig reads chromeExtension.pairedDeviceId from
// ~/.claude.json (le().chromeExtension?.pairedDeviceId).
func persistedDeviceIDFromConfig() string {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return ""
	}
	var m map[string]any
	if jsonUnmarshal(b, &m) != nil {
		return ""
	}
	if ce, ok := m["chromeExtension"].(map[string]any); ok {
		if v, _ := ce["pairedDeviceId"].(string); v != "" {
			return v
		}
	}
	return ""
}

// PersistPairing writes chromeExtension.{pairedDeviceId,pairedDeviceName}
// into ~/.claude.json (Ae() merge in mod_0807.js onExtensionPaired).
func PersistPairing(deviceID, name string) {
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".claude.json")
	var m map[string]any
	if b, err := os.ReadFile(p); err == nil {
		jsonUnmarshal(b, &m)
	}
	if m == nil {
		m = map[string]any{}
	}
	ce, _ := m["chromeExtension"].(map[string]any)
	if ce == nil {
		ce = map[string]any{}
	}
	if cur, _ := ce["pairedDeviceId"].(string); cur == deviceID {
		if curN, _ := ce["pairedDeviceName"].(string); curN == name {
			return
		}
	}
	ce["pairedDeviceId"] = deviceID
	ce["pairedDeviceName"] = name
	m["chromeExtension"] = ce
	b, _ := jsonMarshalIndent(m)
	_ = os.WriteFile(p, b, 0o644)
}

// accountMismatch mirrors the `y` flag in mod_0807.js — set when the OAuth
// token resolves to a different claude.ai account than the persisted login.
var accountMismatch bool

// MarkAccountMismatch records the token-vs-login divergence real reports
// via tengu_chrome_bridge_account_mismatch.
func MarkAccountMismatch() { accountMismatch = true }

// isRemoteSession mirrors Di(): remote/container sessions where "the user's
// computer" is elsewhere.
var isRemoteSession = func() bool {
	return os.Getenv("CLAUDE_CODE_REMOTE") == "true" ||
		os.Getenv("CLAUDE_CODE_REMOTE_SESSION_ID") != "" ||
		os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != ""
}

// disconnectedMessage mirrors onToolCallDisconnected (mod_0807.js, $Ao
// once-wrapper). Order: account mismatch → pinned-remote device → remote
// session → generic.
func (c *Context) disconnectedMessage() string {
	if accountMismatch {
		return "Browser extension is not connected: the OAuth token Claude Code is using belongs to a different claude.ai account than the one Claude Code is logged in as. If CLAUDE_CODE_OAUTH_TOKEN is set in your shell or CI profile, unset it (or re-mint it for this account), then run /logout and /login in Claude Code and make sure the browser extension is signed into the same claude.ai account. If you continue to experience issues, please report a bug: " + bugReportURL
	}
	if c.PairedDeviceIDEnv != "" && persistedDeviceIDFromConfig() == "" {
		return "Browser extension is not connected. The browser chosen for this session could not be reached: the computer running it may be closed or asleep, Chrome may not be running there, or this session may not be able to connect to Chrome. Ask the user to make sure that computer is awake and Chrome is open there, then retry. If it still fails, ask the user to check that the Claude extension in that Chrome is signed in to the same claude.ai account they use here."
	}
	if isRemoteSession() {
		return "Browser extension is not connected. Chrome on the user's computer could not be reached from this session: that computer may be closed or asleep, Chrome may not be running there, the Claude extension may not be installed there (" + installURL + "), or this session may not be able to connect to Chrome. Ask the user to make sure that computer is awake and Chrome is open there with the Claude extension, then retry. If it still fails, ask the user to check that the extension is signed in to the same claude.ai account they use here."
	}
	return "Browser extension is not connected. Please ensure the Claude browser extension is installed and running (" + installURL + "), and that you are logged into claude.ai with the same account as Claude Code. If this is your first time connecting to Chrome, you may need to restart Chrome for the installation to take effect. If you continue to experience issues, please report a bug: " + bugReportURL
}
