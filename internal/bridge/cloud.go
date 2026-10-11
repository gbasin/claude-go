// Cloud → local resume and the session fetch/validate pipeline —
// `claude --teleport <session>` in mod_0686.js (direct mode) →
// teleportWithProgress → cRe → Bx → p3n → uss → WNt → jNt in
// chunk-acxptg39.js (mod_0386.js).
//
// Extracted surfaces reproduced here:
//   - Bx: GET /v1/code/sessions/{id} — 404 "Session not found",
//     401 "Session expired. Please run /login to sign in again.",
//     400 "invalid session ID", other "Failed to fetch session: {status}".
//   - I9r: GET /v1/code/sessions — {data:[...]} list for the bare-flag pick.
//   - Z3e: GET /v1/code/sessions/{id}/teleport-events?limit=1000&cursor=
//     paged (has_more/next_cursor), 404 => the empty-loglines fallback.
//   - J3e: GET /v1/session_ingress/session/{id} — {loglines:[]} fallback.
//   - uss: teleport-events → {type:"user"|"assistant",!isSidechain} filter,
//     last-20000 cap; branch from outcomes[].git_branches[0].source.
//   - p3n: repo validation — match | no_repo_required | not_in_repo |
//     host_unverified | mismatch | error, with real's message text.
//   - WNt: branch checkout — zT name validation, BNt .git verify, Bar fetch
//     (branch:branch, refspec fallback), Har checkout ladder, Uar upstream.
//   - jNt: transcript entries + the "continued from another machine" meta
//     record + "Session resumed"/"Session resumed without branch" notice.
//   - Kps: dirty-worktree refusal before checkout.
//   - Au: UserError — {message, formattedMessage} pair.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gbasin/claude-go/internal/api"
	"github.com/gbasin/claude-go/internal/telemetry"
)

// UserError mirrors Au (TeleportError/UserError): Message is the plain
// string real logs/prints via onDone, UX is the chalk-formatted variant.
// Error() returns UX (the display form) when set.
type UserError struct {
	Message string
	UX      string
}

func (e *UserError) Error() string {
	if e.UX != "" {
		return e.UX
	}
	return e.Message
}

// ---------------------------------------------------------------------------
// CloudSession — the parsed GET /v1/code/sessions/{id} row (Uv fields the
// resume path reads: session_context.sources/.outcomes, environment_kind,
// bound_device_uuid).
// ---------------------------------------------------------------------------

type CloudSession struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	SessionStatus   string `json:"session_status"`
	EnvironmentKind string `json:"environment_kind"`
	EnvironmentID   string `json:"environment_id"`
	BoundDeviceUUID string `json:"bound_device_uuid"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	SessionContext  struct {
		Sources []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"sources"`
		Outcomes []struct {
			Type        string `json:"type"`
			PRURL       string `json:"pr_url"`
			GitBranches []struct {
				Source string `json:"source"`
				Name   string `json:"name"`
			} `json:"git_branches"`
		} `json:"outcomes"`
	} `json:"session_context"`
}

// GetSession mirrors Bx()/oF(): GET /v1/code/sessions/{id}, 15s, OAuth lane.
// Real maps: 401→login text, 404→"Session not found", 400 with
// "invalid session ID"→message, everything else→"Failed to fetch session".
func (c *Client) GetSession(ctx context.Context, id string) (*CloudSession, error) {
	if err := checkID(id, "sessionId"); err != nil {
		return nil, &UserError{Message: fmt.Sprintf("Failed to fetch session: %v", err)}
	}
	r, err := c.withOAuthRetry("GetSession", func(tok string) (*response, error) {
		h, herr := c.codeSessionHeaders(tok)
		if herr != nil {
			return nil, herr
		}
		return c.do(ctx, http.MethodGet,
			c.BaseURL+"/v1/code/sessions/"+codeSessionID(id), nil, h, 15*time.Second)
	})
	if err != nil {
		return nil, err
	}
	switch {
	case r.Status == 200:
	case r.Status == 401:
		return nil, &UserError{Message: "Session expired. Please run /login to sign in again."}
	case r.Status == 404:
		return nil, &UserError{Message: fmt.Sprintf("Session not found: %s", id)}
	case r.Status == 400 && strings.Contains(bodyErrorMessage(r.Body), "invalid session ID"):
		return nil, &UserError{Message: bodyErrorMessage(r.Body)}
	default:
		return nil, &UserError{Message: fmt.Sprintf("Failed to fetch session: %d", r.Status)}
	}
	var v struct {
		Session *CloudSession `json:"session"`
	}
	if json.Unmarshal(r.Body, &v) != nil || v.Session == nil {
		// tolerate a bare session object (older envelopes)
		var s CloudSession
		if json.Unmarshal(r.Body, &s) != nil || s.ID == "" {
			return nil, &UserError{Message: ErrMalformedResponse}
		}
		return &s, nil
	}
	return v.Session, nil
}

// ListCodeSessions mirrors I9r(): GET /v1/code/sessions -> {data:[Uv…]}.
// Used by the bare `claude --teleport` session pick (Pi).
func (c *Client) ListCodeSessions(ctx context.Context) ([]*CloudSession, error) {
	r, err := c.withOAuthRetry("ListCodeSessions", func(tok string) (*response, error) {
		h, herr := c.codeSessionHeaders(tok)
		if herr != nil {
			return nil, herr
		}
		return c.do(ctx, http.MethodGet, c.BaseURL+"/v1/code/sessions", nil, h, 15*time.Second)
	})
	if err != nil {
		return nil, err
	}
	if r.Status != 200 {
		return nil, raise(r.Status, r.Body, "ListCodeSessions", r.Headers)
	}
	var v struct {
		Data []*CloudSession `json:"data"`
	}
	if json.Unmarshal(r.Body, &v) != nil {
		return nil, fmt.Errorf("ListCodeSessions: malformed response")
	}
	return v.Data, nil
}

// ---------------------------------------------------------------------------
// teleport-events / session_ingress — the transcript fetch (uss in
// mod_0386.js).
// ---------------------------------------------------------------------------

// ErrEventsNotFound is Z3e's 404: teleport-events doesn't exist for the
// session → uss falls back to session_ingress loglines.
var ErrEventsNotFound = errors.New("teleport-events not found")

// TeleportEvents mirrors Z3e(): GET .../teleport-events?limit=1000&cursor=
// paginated up to 100 pages. Response envelope is {events:[{payload}],
// has_more, next_cursor}; older entries can be bare payloads, so an item
// without a payload field is kept as-is. Headers: c_t() + x-organization-
// uuid + optional X-Trusted-Device-Token.
func (c *Client) TeleportEvents(ctx context.Context, sessionID string, maxPages int) ([]json.RawMessage, error) {
	if err := checkID(sessionID, "sessionId"); err != nil {
		return nil, err
	}
	if maxPages <= 0 {
		maxPages = 100
	}
	tok, err := c.oauthToken()
	if err != nil {
		return nil, err
	}
	base := c.BaseURL + "/v1/code/sessions/" + codeSessionID(sessionID) + "/teleport-events"
	cursor := ""
	var out []json.RawMessage
	for page := 0; page < maxPages; page++ {
		h, herr := c.codeSessionHeaders(tok)
		if herr != nil {
			return nil, herr
		}
		if c.OrgUUID != "" {
			h.Set("x-organization-uuid", c.OrgUUID)
		}
		u := base + "?limit=1000"
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		r, err := c.do(ctx, http.MethodGet, u, nil, h, 15*time.Second)
		if err != nil {
			return nil, err
		}
		switch r.Status {
		case 200:
		case 404:
			return nil, ErrEventsNotFound
		case 401:
			return nil, &UserError{Message: "Session expired. Please run /login to sign in again."}
		case 403:
			return nil, &UserError{Message: fmt.Sprintf("Access denied to session: %s", sessionID)}
		default:
			return nil, &UserError{Message: fmt.Sprintf("Failed to get session events: %d", r.Status)}
		}
		var v struct {
			Events     []json.RawMessage `json:"events"`
			Data       []json.RawMessage `json:"data"`
			HasMore    bool              `json:"has_more"`
			NextCursor string            `json:"next_cursor"`
			Cursor     string            `json:"cursor"`
		}
		if err := json.Unmarshal(r.Body, &v); err != nil {
			return nil, &UserError{Message: ErrMalformedResponse}
		}
		items := v.Events
		if items == nil {
			items = v.Data
		}
		for _, item := range items {
			var probe struct {
				Payload json.RawMessage `json:"payload"`
			}
			if json.Unmarshal(item, &probe) == nil && len(probe.Payload) > 0 {
				out = append(out, probe.Payload)
			} else {
				out = append(out, item)
			}
		}
		next := v.NextCursor
		if next == "" {
			next = v.Cursor
		}
		if !v.HasMore || next == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

// IngressLog mirrors J3e(): GET /v1/session_ingress/session/{id} —
// {loglines:[...]}, 404 means none.
func (c *Client) IngressLog(ctx context.Context, sessionID string) ([]json.RawMessage, error) {
	if err := checkID(sessionID, "sessionId"); err != nil {
		return nil, err
	}
	tok, err := c.oauthToken()
	if err != nil {
		return nil, err
	}
	h, err := c.codeSessionHeaders(tok)
	if err != nil {
		return nil, err
	}
	if c.OrgUUID != "" {
		h.Set("x-organization-uuid", c.OrgUUID)
	}
	r, err := c.do(ctx, http.MethodGet,
		c.BaseURL+"/v1/session_ingress/session/"+codeSessionID(sessionID), nil, h, 15*time.Second)
	if err != nil {
		return nil, err
	}
	if r.Status == 404 {
		return nil, nil
	}
	if r.Status != 200 {
		return nil, &UserError{Message: fmt.Sprintf("Failed to get session log: %d", r.Status)}
	}
	var v struct {
		Loglines []json.RawMessage `json:"loglines"`
	}
	if err := json.Unmarshal(r.Body, &v); err != nil {
		return nil, &UserError{Message: ErrMalformedResponse}
	}
	return v.Loglines, nil
}

// SessionLog is uss()'s return: the filtered log entries plus the outcome
// branch (first git_worktree outcome's git_branches[0].source // pr_url)
// and the fetched session's environmentKind.
type SessionLog struct {
	Entries         []json.RawMessage
	Branch          string
	EnvironmentKind string
}

// teleMaxLog mirrors x3e in the real source (last 20000 entries kept).
const teleMaxLog = 20000

// FetchSessionLog mirrors uss(). Entries with a "type" field are filtered
// to user/assistant and non-sidechain.
func (c *Client) FetchSessionLog(ctx context.Context, sess *CloudSession) (*SessionLog, error) {
	raw, err := c.TeleportEvents(ctx, sess.ID, 100)
	if errors.Is(err, ErrEventsNotFound) {
		logs, lerr := c.IngressLog(ctx, sess.ID)
		if lerr != nil {
			return nil, lerr
		}
		return &SessionLog{Entries: logs, EnvironmentKind: "unknown"}, nil
	}
	if err != nil {
		return nil, err
	}
	var keep []json.RawMessage
	for _, e := range raw {
		var probe struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
		}
		if json.Unmarshal(e, &probe) != nil {
			continue
		}
		if (probe.Type == "user" || probe.Type == "assistant") && !probe.IsSidechain {
			keep = append(keep, e)
		}
	}
	if len(keep) > teleMaxLog {
		keep = keep[len(keep)-teleMaxLog:]
	}
	var branch string
	for _, o := range sess.SessionContext.Outcomes {
		if o.Type != "git_worktree" {
			continue
		}
		if len(o.GitBranches) > 0 {
			branch = o.GitBranches[0].Source
			if branch == "" {
				branch = o.GitBranches[0].Name
			}
		}
		if branch == "" {
			branch = o.PRURL
		}
		break
	}
	return &SessionLog{Entries: keep, Branch: branch, EnvironmentKind: sess.EnvironmentKind}, nil
}

// ---------------------------------------------------------------------------
// p3n — session repo validation against the cwd's git remote.
// ---------------------------------------------------------------------------

// GitRemote is oP()'s parse result: host/owner/name for http(s) and
// scp-style (git@host:owner/name) remotes.
type GitRemote struct {
	Host, Owner, Name string
}

// oP()'s regexes (mod_0116.js) — exactly two path segments; segments must
// pass c() (H): alphanumerics plus ._-, no leading -, not "." or "..";
// host must pass m(): alphanumerics plus .-
var (
	scpRemoteRe = regexp.MustCompile(`^git@([^:/@]+):([^/]+)/([^/]+?)(\.git)?/?$`)
	urlRemoteRe = regexp.MustCompile(`^(https?|ssh|git)://(?:[^@/?#]*@)?([^/:?#@]+(?::\d+)?)/([^/]+)/([^/]+?)(\.git)?/?$`)
	pathSegRe   = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	hostRe      = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
	// oeo(): the owner/name tail of any URL-shaped remote — used when the
	// full oP parse (host included) fails.
	ownerNameRe = regexp.MustCompile(`(?:[:/]|^)([^/:]+)/([^/:]+?)(\.git)?/?$`)
	schemeRe    = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://[^/]*`)
)

// validSeg mirrors c() — the path-segment validator.
func validSeg(s string) bool {
	return pathSegRe.MatchString(s) && !strings.HasPrefix(s, "-") &&
		s != "." && s != ".."
}

func validHost(h string) bool {
	return hostRe.MatchString(h) && !strings.HasPrefix(h, "-") &&
		h != "." && h != ".."
}

// ParseGitRemote mirrors oP(). For http(s) URLs the host keeps its port
// (real's `l==="https"||l==="http"?s:u`); for ssh/git it's stripped.
func ParseGitRemote(raw string) *GitRemote {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, " \t\n") {
		return nil
	}
	if m := scpRemoteRe.FindStringSubmatch(raw); m != nil {
		if !validHost(m[1]) || !validSeg(m[2]) || !validSeg(m[3]) {
			return nil
		}
		return &GitRemote{Host: m[1], Owner: m[2], Name: m[3]}
	}
	if m := urlRemoteRe.FindStringSubmatch(raw); m != nil {
		hostport := m[2]
		host := hostport
		if i := strings.Index(hostport, ":"); i >= 0 {
			host = hostport[:i]
		}
		if !validHost(host) || !validSeg(m[3]) || !validSeg(m[4]) {
			return nil
		}
		if m[1] == "https" || m[1] == "http" {
			return &GitRemote{Host: hostport, Owner: m[3], Name: m[4]}
		}
		return &GitRemote{Host: host, Owner: m[3], Name: m[4]}
	}
	return nil
}

// parseOwnerName mirrors oeo(): strip trailing slashes and the
// scheme://authority prefix, then take the last two path-ish segments.
func parseOwnerName(raw string) *GitRemote {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	s = schemeRe.ReplaceAllString(s, "")
	m := ownerNameRe.FindStringSubmatch(s)
	if m == nil || !validSeg(m[1]) || !validSeg(m[2]) {
		return nil
	}
	return &GitRemote{Owner: m[1], Name: m[2]}
}

// parseSessionRepo mirrors `g?`${owner}/${name}`:I6e(url)`: an oP parse
// wins; otherwise I6e accepts only a bare "owner/name" string (its
// github.com-gated oP retry is unreachable here since g already failed).
func parseSessionRepo(raw string) (slug string, g *GitRemote) {
	g = ParseGitRemote(raw)
	if g != nil {
		return g.slug(), g
	}
	o := strings.TrimSpace(raw)
	if !strings.Contains(o, "://") && !strings.Contains(o, "@") &&
		strings.Contains(o, "/") {
		parts := strings.Split(o, "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			name := strings.TrimSuffix(parts[1], ".git")
			if validSeg(parts[0]) && validSeg(name) {
				return parts[0] + "/" + name, nil
			}
		}
	}
	return "", nil
}

func (r *GitRemote) slug() string { return r.Owner + "/" + r.Name }

// normHost mirrors lve()/tGe(): hostname normalized lowercase with the
// www. prefix and port stripped.
func normHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.Index(h, ":"); i >= 0 {
		h = h[:i]
	}
	for strings.HasPrefix(h, "www.") {
		h = h[4:]
	}
	return h
}

// isGitRepo: `git rev-parse --is-inside-work-tree`.
func isGitRepo(cwd string) bool {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--is-inside-work-tree").Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// gitRemoteURL (defined in cmd.go) mirrors Iar(dHe(cwd)): the configured
// remote.origin.url — read directly so an unparseable value is preserved
// for the error text.

// currentGitRemote mirrors PT(): the parsed origin remote, or nil. PM()
// (owner containing "/") results are rejected — PT never returns them.
func currentGitRemote(cwd string) *GitRemote {
	if !isGitRepo(cwd) {
		return nil
	}
	r := ParseGitRemote(gitRemoteURL(cwd))
	if r == nil || strings.Contains(r.Owner, "/") {
		return nil
	}
	return r
}

// pushGitRemote mirrors ZZr(): `git remote get-url --push origin` — the
// second-chance lookup when the slug matches but the origin host differs.
func pushGitRemote(cwd string) *GitRemote {
	out, err := exec.Command("git", "-C", cwd, "remote", "get-url", "--push", "origin").Output()
	if err != nil {
		return nil
	}
	return ParseGitRemote(strings.TrimSpace(string(out)))
}

// RepoStatus is p3n()'s result. Fields follow the real result verbatim.
type RepoStatus struct {
	Status       string // match|no_repo_required|not_in_repo|host_unverified|mismatch|error
	SessionRepo  string // owner/name of the session's repo
	SessionHost  string // host of the session's repo (parseable urls)
	CurrentRepo  string // owner/name of cwd's repo (when known)
	CurrentHost  string
	RawRemoteURL string // unparseable remote url, for the error text
	ErrorMessage string // status=="error"
}

// ValidateSessionRepo mirrors p3n(e).
func ValidateSessionRepo(cwd string, sess *CloudSession) RepoStatus {
	n := currentGitRemote(cwd)
	var r string
	if n != nil {
		r = n.slug()
	}
	var srcURL string
	for _, s := range sess.SessionContext.Sources {
		if s.Type == "git_repository" && s.URL != "" {
			srcURL = s.URL
			break
		}
	}
	if srcURL == "" {
		return RepoStatus{Status: "no_repo_required"}
	}
	h, g := parseSessionRepo(srcURL)
	if h == "" {
		return RepoStatus{Status: "no_repo_required"}
	}
	if r == "" {
		if isGitRepo(cwd) {
			raw := gitRemoteURL(cwd)
			if b := parseOwnerName(raw); b != nil {
				st := RepoStatus{
					SessionRepo: h, SessionHost: hostOrEmpty(g),
					CurrentRepo: b.slug(), RawRemoteURL: raw,
				}
				if strings.EqualFold(b.slug(), h) {
					st.Status = "host_unverified"
				} else {
					st.Status = "mismatch"
					st.CurrentHost = b.Host
				}
				return st
			}
			if raw != "" {
				return RepoStatus{Status: "not_in_repo", SessionRepo: h,
					SessionHost: hostOrEmpty(g), RawRemoteURL: raw}
			}
		}
		return RepoStatus{Status: "not_in_repo", SessionRepo: h,
			SessionHost: hostOrEmpty(g)}
	}
	match := strings.EqualFold(r, h)
	hostOK := g == nil || n == nil ||
		normHost(n.Host) == normHost(g.Host)
	if match && hostOK {
		return RepoStatus{Status: "match", SessionRepo: h, CurrentRepo: r}
	}
	if g != nil {
		if alt := pushGitRemote(cwd); alt != nil &&
			normHost(alt.Host) == normHost(g.Host) &&
			strings.EqualFold(alt.slug(), h) {
			return RepoStatus{Status: "match", SessionRepo: h, CurrentRepo: alt.slug()}
		}
	}
	return RepoStatus{Status: "mismatch", SessionRepo: h, CurrentRepo: r,
		SessionHost: hostOrEmpty(g), CurrentHost: n.Host}
}

func hostOrEmpty(g *GitRemote) string {
	if g == nil {
		return ""
	}
	return g.Host
}

// RepoDisplays mirrors f3n(): the session/current display strings for the
// mismatch message (host prefix added only when the hosts differ).
func RepoDisplays(st RepoStatus) (sessionDisplay, currentDisplay string) {
	sessionDisplay, currentDisplay = st.SessionRepo, st.CurrentRepo
	if st.SessionHost != "" && st.CurrentHost != "" &&
		normHost(st.SessionHost) != normHost(st.CurrentHost) {
		sessionDisplay = st.SessionHost + "/" + st.SessionRepo
		currentDisplay = st.CurrentHost + "/" + st.CurrentRepo
	}
	return sessionDisplay, currentDisplay
}

// hostPrefix mirrors the `sessionHost && !uo(sessionHost)` prefix: bare
// github.com gets folded into owner/name display; other hosts are shown.
func hostPrefix(st RepoStatus) string {
	if st.SessionHost != "" && normHost(st.SessionHost) != "github.com" {
		return st.SessionHost + "/" + st.SessionRepo
	}
	return st.SessionRepo
}

// repoStatusError mirrors the cRe/direct-mode status→error switch.
// sessionID is the arg verbatim (real splices it into the text).
func repoStatusError(st RepoStatus, sessionID string) error {
	switch st.Status {
	case "match", "no_repo_required", "host_unverified":
		// host_unverified is a confirmation gate in real (ki()); claude-go's
		// noninteractive flag path proceeds — the remote name check ran.
		return nil
	case "not_in_repo":
		w := hostPrefix(st)
		extra := ""
		if st.RawRemoteURL != "" {
			extra = "\nCouldn't parse your git remote: " + st.RawRemoteURL
		}
		return &UserError{
			Message: fmt.Sprintf("You must run claude --teleport %s from a checkout of %s.%s",
				sessionID, w, extra),
			UX: fmt.Sprintf("Error: You must run claude --teleport %s from a checkout of %s.%s",
				sessionID, w, extra),
		}
	case "mismatch":
		sd, cd := RepoDisplays(st)
		extra := ""
		if cd != "" {
			extra = "\nThis repo is " + cd + "."
		}
		return &UserError{
			Message: fmt.Sprintf("You must run claude --teleport %s from a checkout of %s.%s",
				sessionID, sd, extra),
			UX: fmt.Sprintf("Error: You must run claude --teleport %s from a checkout of %s.%s",
				sessionID, sd, extra),
		}
	case "error":
		m := st.ErrorMessage
		if m == "" {
			m = "Failed to validate session"
		}
		return &UserError{Message: m, UX: "Error: " + m}
	default:
		return fmt.Errorf("Unhandled repo validation status: %s", st.Status)
	}
}

// ---------------------------------------------------------------------------
// WNt — checkout the cloud session's outcome branch.
// ---------------------------------------------------------------------------

var branchNameRe = regexp.MustCompile(`^[a-zA-Z0-9/._+@-]+$`)

// ValidBranchName mirrors zT(): reject empty, leading -/, "..", and
// empty/dot path segments; allow alphanumerics + /._+@-.
func ValidBranchName(b string) bool {
	if b == "" || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") {
		return false
	}
	if strings.Contains(b, "..") {
		return false
	}
	for _, seg := range strings.Split(b, "/") {
		if seg == "" || seg == "." {
			return false
		}
	}
	return branchNameRe.MatchString(b)
}

// gitCmd runs git in cwd and returns (exit code, stdout, stderr).
func gitCmd(cwd string, args ...string) (int, string, string) {
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	var so, se strings.Builder
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return code, so.String(), se.String()
}

// IsGitRepoClean mirrors U6e({ignoreUntracked:!0}): tracked-file dirt only.
func IsGitRepoClean(cwd string) bool {
	code, out, _ := gitCmd(cwd, "status", "--porcelain", "--untracked-files=no")
	return code == 0 && strings.TrimSpace(out) == ""
}

// CurrentBranch mirrors Ome(): `git branch --show-current`.
func CurrentBranch(cwd string) string {
	_, out, _ := gitCmd(cwd, "branch", "--show-current")
	return strings.TrimSpace(out)
}

// verifyCheckout mirrors BNt(): the checkout must have a .git entry that
// points back to it — a .git directory, or a .git file with a
// `gitdir: <path>` pointer that exists (worktrees/submodules).
func verifyCheckout(cwd string) error {
	entry := filepath.Join(cwd, ".git")
	st, err := os.Lstat(entry)
	if err != nil {
		return &UserError{
			Message: fmt.Sprintf("Could not verify this checkout's git repository: %s has no .git entry that points back to it", cwd),
			UX:      "Error: Could not verify this checkout's git repository (no .git entry that points back to it)",
		}
	}
	if st.IsDir() {
		return nil
	}
	if st.Mode()&os.ModeSymlink != 0 {
		if _, err := os.Stat(entry); err == nil {
			return nil
		}
	} else if st.Mode().IsRegular() {
		if b, err := os.ReadFile(entry); err == nil {
			s := strings.TrimSpace(string(b))
			if rest, ok := strings.CutPrefix(s, "gitdir:"); ok {
				p := strings.TrimSpace(rest)
				if !filepath.IsAbs(p) {
					p = filepath.Join(cwd, p)
				}
				if _, err := os.Stat(p); err == nil {
					return nil
				}
			}
		}
	}
	return &UserError{
		Message: fmt.Sprintf("Could not verify this checkout's git repository: %s has no .git entry that points back to it", cwd),
		UX:      "Error: Could not verify this checkout's git repository (no .git entry that points back to it)",
	}
}

// fetchOrigin mirrors Bar(): `git fetch origin <b>:<b>`, and on a refspec
// complaint the plain-ref fallback `git fetch origin <b>`.
func fetchOrigin(cwd, branch string) {
	code, _, stderr := gitCmd(cwd, "fetch", "origin", branch+":"+branch)
	if code != 0 && branch != "" && strings.Contains(stderr, "refspec") {
		gitCmd(cwd, "fetch", "origin", branch)
	}
}

// checkoutBranch mirrors Har() + Uar(): the three-step checkout ladder,
// then upstream repair (`rev-parse --abbrev-ref <b>@{upstream}`, verify
// origin/<b>, `branch --set-upstream-to origin/<b> <b>`).
func checkoutBranch(cwd, branch string) error {
	code, _, stderr := gitCmd(cwd, "checkout", branch, "--")
	if code != 0 {
		code, _, stderr = gitCmd(cwd, "checkout", "-b", branch, "--track", "origin/"+branch, "--")
	}
	if code != 0 {
		code, _, stderr = gitCmd(cwd, "checkout", "--track", "origin/"+branch, "--")
	}
	if code != 0 {
		telemetry.Event("tengu_teleport_error_branch_checkout_failed", nil)
		return &UserError{
			Message: fmt.Sprintf("Failed to checkout branch '%s': %s", branch, strings.TrimSpace(stderr)),
			UX:      fmt.Sprintf("Failed to checkout branch '%s'", branch),
		}
	}
	// Uar — set upstream when unset and the remote ref exists.
	if code, _, _ := gitCmd(cwd, "rev-parse", "--abbrev-ref", branch+"@{upstream}"); code != 0 {
		if code, _, _ := gitCmd(cwd, "rev-parse", "--verify", "origin/"+branch); code == 0 {
			gitCmd(cwd, "branch", "--set-upstream-to", "origin/"+branch, branch)
		}
	}
	return nil
}

// CheckoutSessionBranch mirrors WNt(e): validates the branch name, verifies
// the checkout, fetches, checks out, repairs upstream. branch=="" stays on
// the current branch. Errors are returned (real catches them into
// {branchName, branchError}).
func CheckoutSessionBranch(cwd, branch string) error {
	if branch == "" {
		return nil
	}
	if !ValidBranchName(branch) {
		return &UserError{
			Message: fmt.Sprintf("Invalid branch name from cloud session: %s", branch),
			UX:      "Error: Invalid branch name from cloud session",
		}
	}
	if err := verifyCheckout(cwd); err != nil {
		return err
	}
	fetchOrigin(cwd, branch)
	return checkoutBranch(cwd, branch)
}

// ---------------------------------------------------------------------------
// jNt — turn the fetched log into the resumed message list.
// ---------------------------------------------------------------------------

// ResumeMessages mirrors jNt(log, branchError, envKind): the transcript's
// user/assistant entries (remoteSourced flagging dropped — Go carries the
// raw record through), then the isMeta continuation record. The
// "Session resumed [without branch: …]" notice is returned separately —
// real renders it as a suggestion/warning line, not a model message.
func ResumeMessages(entries []json.RawMessage, cwd string) []api.Message {
	var msgs []api.Message
	for _, e := range entries {
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(e, &rec) != nil {
			continue
		}
		role := rec.Message.Role
		if role == "" {
			role = rec.Type
		}
		if len(rec.Message.Content) == 0 {
			continue
		}
		msgs = append(msgs, api.Message{Role: role, Content: rec.Message.Content})
	}
	// Dar() — the isMeta record real injects before the resume marker.
	msgs = append(msgs, api.TextMessage("user",
		"This session is being continued from another machine. "+
			"Application state may have changed. The updated working directory is "+cwd))
	return msgs
}

// ResumeNotice mirrors Oar(branchError): the display line after resume.
func ResumeNotice(branchError error) string {
	if branchError == nil {
		return "Session resumed"
	}
	var ue *UserError
	if errors.As(branchError, &ue) && ue.UX != "" {
		return fmt.Sprintf("Session resumed without branch: %s", ue.UX)
	}
	return fmt.Sprintf("Session resumed without branch: %s", branchError.Error())
}

// ---------------------------------------------------------------------------
// ResumeFromCloud — the direct `--teleport <session>` pipeline
// (mod_0686.js direct branch → teleportWithProgress → cRe).
// ---------------------------------------------------------------------------

// ResumeDeps mirrors the injectable boundaries; nil fields take the real
// defaults (cwd = process cwd).
type ResumeDeps struct {
	CWD string
}

// ResumeResult is what teleportWithProgress hands back (j = {branch, log,
// environmentKind} + the converted message list).
type ResumeResult struct {
	Messages        []api.Message
	Branch          string
	BranchError     error
	EnvironmentKind string
	Session         *CloudSession
}

// ResumeFromCloud runs cRe's client-verifiable sequence:
//
//	dirty-worktree guard (Kps) → Bx → p3n → uss → WNt → jNt
//
// The orgUUID gate (z2(), "Unable to get organization UUID for
// constructing session URL") applies only to the session_ingress fallback
// in real — the ingress host needs it; c.OrgUUID is read from the client.
func (c *Client) ResumeFromCloud(ctx context.Context, sessionID string, d *ResumeDeps) (*ResumeResult, error) {
	cwd := ""
	if d != nil {
		cwd = d.CWD
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	// cRe's token gate (mod_0686.js): mn()?.accessToken must exist before
	// Bx runs; API-key auth doesn't qualify.
	if _, err := c.oauthToken(); err != nil {
		return nil, &UserError{Message: "Claude Code cloud sessions require authentication with a Claude.ai account. " +
			"API key authentication is not sufficient. Please run /login to authenticate, " +
			"or check your authentication status with /status."}
	}
	sess, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	st := ValidateSessionRepo(cwd, sess)
	if err := repoStatusError(st, sessionID); err != nil {
		return nil, err
	}
	log, err := c.FetchSessionLog(ctx, sess)
	if err != nil {
		return nil, err
	}
	// Kps runs before the checkout ladder: a dirty tree is a hard refuse.
	var branchErr error
	if log.Branch != "" {
		if isGitRepo(cwd) && !IsGitRepoClean(cwd) {
			telemetry.Event("tengu_teleport_error_git_not_clean", nil)
			return nil, &UserError{
				Message: "Git working directory is not clean. Please commit or stash your changes before using --teleport.",
				UX:      "Error: Git working directory is not clean. Please commit or stash your changes before using --teleport.",
			}
		}
		branchErr = CheckoutSessionBranch(cwd, log.Branch)
	}
	return &ResumeResult{
		Messages:        ResumeMessages(log.Entries, cwd),
		Branch:          log.Branch,
		BranchError:     branchErr,
		EnvironmentKind: log.EnvironmentKind,
		Session:         sess,
	}, nil
}
