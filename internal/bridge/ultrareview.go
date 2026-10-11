// Ultrareview — the bundled BUGHUNTER cloud-review flow (mod_1006.js
// jNn/WNn/GNn/DLe + mod_1007.js command wrapper, config accessors in
// mod_0278.js's tengu_review_bughunter_config).
//
// Extracted and implemented:
//   - GET /v1/ultrareview/preflight (de): OAuth lane, 5s timeout, the
//     {action: proceed|confirm|blocked, billing_note, github_login,
//     confirm{title,body}, blocked{message,action_url,reason}} schema and
//     the CLAUDE_CODE_ULTRAREVIEW_PREFLIGHT_FIXTURE override.
//   - The WNn resolution ladder: unavailable → proceed, blocked → block,
//     confirm → needs-confirm unless overage was already confirmed this
//     session.
//   - The BUGHUNTER_* environment set + env_011111111111111111111113 env id
//   - the N(value, default, cap) clamps (GNn).
//   - PR mode (refs/pull/<N>/head source, BUGHUNTER_PR_NUMBER/REPOSITORY)
//     and branch mode (git bundle upload → seed_bundle_file_id,
//     BUGHUNTER_BASE_BRANCH=<merge-base>).
//
// Honest gaps (unextracted or not replicated):
//   - The bundle pipeline (real's $Jt/tze) captures uncommitted work into
//     synthetic WIP commits, verifies with `git bundle list-heads`, and has
//     a hardened/legacy fallback lane. We ship the simple `git bundle
//     create <base>..HEAD` subset — WIP files are not included.
//   - The low-credits line (he: usage-credits read via /api/oauth/usage)
//     and the github-access precheck (JQt) are skipped.
//   - transport reasons "essential-traffic-only" has no local detector;
//     non-first-party maps to data_residency, missing OAuth to
//     no_oauth_token, everything else to "unavailable → proceed".
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gbasin/claude-go/internal/api"
)

// bughunterEnvironmentID is the review environment (S in GNn).
const bughunterEnvironmentID = "env_011111111111111111111113"

// UltrareviewPreflightFixtureEnv mirrors CLAUDE_CODE_ULTRAREVIEW_PREFLIGHT_FIXTURE.
const UltrareviewPreflightFixtureEnv = "CLAUDE_CODE_ULTRAREVIEW_PREFLIGHT_FIXTURE"

// UltrareviewPreflight is the GET /v1/ultrareview/preflight response
// (ue() zod schema in mod_1006.js).
type UltrareviewPreflight struct {
	Action      string  `json:"action"` // "proceed" | "confirm" | "blocked"
	BillingNote *string `json:"billing_note,omitempty"`
	GithubLogin *string `json:"github_login,omitempty"`
	Confirm     *struct {
		Title string `json:"title,omitempty"`
		Body  string `json:"body"`
	} `json:"confirm,omitempty"`
	Blocked *struct {
		Message   string  `json:"message"`
		ActionURL *string `json:"action_url"`
		Reason    string  `json:"reason,omitempty"`
	} `json:"blocked,omitempty"`
}

func blockedPreflight(message, reason string) *UltrareviewPreflight {
	return &UltrareviewPreflight{
		Action: "blocked",
		Blocked: &struct {
			Message   string  `json:"message"`
			ActionURL *string `json:"action_url"`
			Reason    string  `json:"reason,omitempty"`
		}{Message: message, Reason: reason},
	}
}

// FetchUltrareviewPreflight is de(): fixture env first, then the real GET.
// nil return = preflight unavailable → real proceeds anyway
// (WNn marks it preflightUnavailable).
func (c *Client) FetchUltrareviewPreflight(ctx context.Context) *UltrareviewPreflight {
	if fx := os.Getenv(UltrareviewPreflightFixtureEnv); fx != "" {
		var pf UltrareviewPreflight
		if json.Unmarshal([]byte(fx), &pf) == nil {
			return &pf
		}
		return nil
	}
	if api.DetectProvider() != api.ProviderFirstParty {
		// real's data-residency transport reason
		return blockedPreflight(
			"Ultrareview runs in a cloud session and is unavailable on third-party providers.",
			"data_residency")
	}
	tok, err := c.oauthToken()
	if err != nil {
		return blockedPreflight(
			"Ultrareview requires a Claude.ai account. Run /login to authenticate.",
			"no_oauth_token")
	}
	h, err := c.codeSessionHeaders(tok)
	if err != nil {
		return nil
	}
	h.Set("x-client-request-id", newUUIDv4())
	r, err := c.do(ctx, http.MethodGet, c.BaseURL+"/v1/ultrareview/preflight", nil, h, 5*time.Second)
	if err != nil {
		c.debugf("[ultrareview] preflight request failed: %v", err)
		return nil
	}
	if r.Status != 200 {
		c.debugf("[ultrareview] preflight %d: %s", r.Status, bodyErrorMessage(r.Body))
		return nil
	}
	var pf UltrareviewPreflight
	if err := json.Unmarshal(r.Body, &pf); err != nil || pf.Action == "" {
		// real: schema mismatch → telemetry + nil (proceed)
		c.debugf("[ultrareview] preflight schema mismatch")
		return nil
	}
	return &pf
}

// PreflightKind is the WNn outcome.
type PreflightKind int

const (
	PreflightProceed      PreflightKind = iota // includes preflightUnavailable
	PreflightBlocked                           // server/policy refusal
	PreflightNeedsConfirm                      // billing consent required
)

// PreflightOutcome is the resolved preflight decision (WNn).
type PreflightOutcome struct {
	Kind                 PreflightKind
	PreflightUnavailable bool   // GET failed/schema-mismatched
	BillingNote          string // billing_note passthrough
	GithubLogin          string // github_login passthrough
	Reason               string // blocked.reason
	Message              string // blocked.message / confirm body
	ActionURL            string // blocked.action_url
}

// ResolveUltrareviewPreflight mirrors WNn: a failed preflight proceeds
// (flagged unavailable), "confirm" requires overage consent unless already
// granted this session.
func ResolveUltrareviewPreflight(pf *UltrareviewPreflight, overageConfirmed bool) PreflightOutcome {
	if pf == nil {
		return PreflightOutcome{Kind: PreflightProceed, PreflightUnavailable: true}
	}
	out := PreflightOutcome{}
	if pf.BillingNote != nil {
		out.BillingNote = *pf.BillingNote
	}
	if pf.GithubLogin != nil {
		out.GithubLogin = *pf.GithubLogin
	}
	switch pf.Action {
	case "blocked":
		out.Kind = PreflightBlocked
		if pf.Blocked != nil {
			out.Message = pf.Blocked.Message
			out.Reason = pf.Blocked.Reason
			if pf.Blocked.ActionURL != nil {
				out.ActionURL = *pf.Blocked.ActionURL
			}
		}
		if out.Message == "" {
			out.Message = "Ultrareview is unavailable for your organization."
		}
		if out.Reason == "" {
			out.Reason = "server"
		}
	case "confirm":
		if overageConfirmed {
			out.Kind = PreflightProceed
			return out
		}
		out.Kind = PreflightNeedsConfirm
		// real appends a low-credits line when usage reads show < typical
		// cost (he()) — that read is unextracted here; body is the
		// confirm.body text when present, else the standard billing line.
		if pf.Confirm != nil && pf.Confirm.Body != "" {
			out.Message = pf.Confirm.Body
		} else {
			out.Message = "This review bills as usage credits (" + BughunterCostNote() + ")."
		}
	default: // "proceed" and anything unrecognized
		out.Kind = PreflightProceed
	}
	return out
}

// ---------------------------------------------------------------------------
// tengu_review_bughunter_config (ASe() in mod_0278.js) + BUGHUNTER_* env.
// ---------------------------------------------------------------------------

// BughunterConfig is the flag payload; the json tags are the wire keys real
// reads off the tengu_review_bughunter_config feature value.
type BughunterConfig struct {
	Enabled                     *bool   `json:"enabled"`
	CodeQuality                 *bool   `json:"code_quality"`
	FleetSize                   float64 `json:"fleet_size"`
	MaxDurationMinutes          float64 `json:"max_duration_minutes"`
	AgentTimeoutSeconds         float64 `json:"agent_timeout_seconds"`
	TotalWallclockMinutes       float64 `json:"total_wallclock_minutes"`
	ClientWaitMinutes           float64 `json:"client_wait_minutes"`
	Model                       string  `json:"model"`
	CostNoteText                string  `json:"cost_note"`
	DurationNote                string  `json:"duration_note"`
	TypicalCostUSD              float64 `json:"typical_cost_usd"`
	MaxDiffFiles                float64 `json:"max_diff_files"`
	MaxDiffLines                float64 `json:"max_diff_lines"`
	EmptyTreeFallbackEnabled    *bool   `json:"empty_tree_fallback_enabled"`
	GithubAccessPrecheckEnabled *bool   `json:"github_access_precheck_enabled"`
}

// LoadBughunterConfig overlays the remote flag (nil/absent → zero config,
// which yields the same defaults real's accessors fall back to).
func LoadBughunterConfig() BughunterConfig {
	var cfg BughunterConfig
	if m, ok := RemoteConfigSource("tengu_review_bughunter_config"); ok {
		overlayJSON(&cfg, m)
	}
	return cfg
}

// BughunterEnabled mirrors Qft(): the flag must say enabled:true (a present
// flag with enabled:false is the kill switch; absent flag defaults to on
// since remote config is unavailable in this build).
func (c BughunterConfig) EnabledGate() bool {
	return c.Enabled == nil || *c.Enabled
}

// CostNote is Rue(): cost_note or "$10-$20".
func (c BughunterConfig) CostNote() string {
	if c.CostNoteText != "" {
		return c.CostNoteText
	}
	return "$10-$20"
}

// DurationNote is _3(): duration_note or "~10–20 min".
func (c BughunterConfig) Duration() string {
	if c.DurationNote != "" {
		return c.DurationNote
	}
	return "~10–20 min"
}

// ClientWaitMinutes is nYe(): client_wait_minutes clamped to [30,55],
// default 45.
func (c BughunterConfig) ClientWait() int {
	v := c.ClientWaitMinutes
	if v >= 30 && v <= 55 {
		return int(v)
	}
	return 45
}

// clampN mirrors N(k, def, cap) in GNn: non-finite/non-positive/out-of-cap
// all fall back to the default (note: >cap returns the DEFAULT, not cap).
func clampN(v float64, def, cap int) int {
	if v <= 0 {
		return def
	}
	i := int(v)
	if i > cap {
		return def
	}
	return i
}

// BughunterEnv builds the T env-var map in GNn:
//
//	BUGHUNTER_DRY_RUN=1, AUTOPATCH_ENABLE_ASIMOV=1,
//	BUGHUNTER_CODE_QUALITY=1 (unless config.code_quality===false),
//	BUGHUNTER_FLEET_SIZE (def 5, cap 20),
//	BUGHUNTER_MAX_DURATION (def 10min, cap 25),
//	BUGHUNTER_AGENT_TIMEOUT (def 600s, cap 1800),
//	BUGHUNTER_TOTAL_WALLCLOCK (def 22min, cap 27),
//	BUGHUNTER_RESULT_RELAY_MAX_SEC = (client_wait-3)*60,
//	BUGHUNTER_MODEL when configured,
//	BUGHUNTER_DEV_BUNDLE_B64 passthrough when set in the environment.
func (c BughunterConfig) Env() map[string]string {
	env := map[string]string{
		"BUGHUNTER_DRY_RUN":              "1",
		"AUTOPATCH_ENABLE_ASIMOV":        "1",
		"BUGHUNTER_FLEET_SIZE":           strconv.Itoa(clampN(c.FleetSize, 5, 20)),
		"BUGHUNTER_MAX_DURATION":         strconv.Itoa(clampN(c.MaxDurationMinutes, 10, 25)),
		"BUGHUNTER_AGENT_TIMEOUT":        strconv.Itoa(clampN(c.AgentTimeoutSeconds, 600, 1800)),
		"BUGHUNTER_TOTAL_WALLCLOCK":      strconv.Itoa(clampN(c.TotalWallclockMinutes, 22, 27)),
		"BUGHUNTER_RESULT_RELAY_MAX_SEC": strconv.Itoa((c.ClientWait() - 3) * 60),
	}
	if c.CodeQuality == nil || *c.CodeQuality {
		env["BUGHUNTER_CODE_QUALITY"] = "1"
	}
	if c.Model != "" {
		env["BUGHUNTER_MODEL"] = c.Model
	}
	if v := os.Getenv("BUGHUNTER_DEV_BUNDLE_B64"); v != "" {
		env["BUGHUNTER_DEV_BUNDLE_B64"] = v
	}
	return env
}

// BughunterCostNote is the package-level convenience for the flag-default
// cost note.
func BughunterCostNote() string { return LoadBughunterConfig().CostNote() }

// BughunterEnvironmentID exposes the review environment id.
func BughunterEnvironmentID() string { return bughunterEnvironmentID }

// ---------------------------------------------------------------------------
// Git-bundle seed upload — the honest subset of real's $Jt/tze.
// `git bundle create <tmp> <base>..HEAD`, `git bundle list-heads` sanity,
// POST /v1/files multipart (file + purpose=user_data) → fileId.
// ---------------------------------------------------------------------------

// UploadGitBundle creates a git bundle of <base>..HEAD in cwd and uploads
// it to the files API, returning the file_id for seed_bundle_file_id.
// Real additionally packs uncommitted work and verifies heads; flagged via
// the returned warning when the tree is dirty.
func (c *Client) UploadGitBundle(ctx context.Context, cwd, baseRef string) (fileID string, warning string, err error) {
	tmp, err := os.CreateTemp("", "claude-ultrareview-*.bundle")
	if err != nil {
		return "", "", err
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)

	rangeSpec := "HEAD"
	if baseRef != "" {
		rangeSpec = baseRef + "..HEAD"
	}
	out, err := exec.CommandContext(ctx, "git", "-C", cwd,
		"-c", "submodule.recurse=false",
		"bundle", "create", path, rangeSpec, "--").CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("git bundle create failed: %s", strings.TrimSpace(string(out)))
	}
	// sanity: the bundle must carry at least one head
	if heads, herr := exec.CommandContext(ctx, "git", "bundle", "list-heads", path).Output(); herr != nil || len(heads) == 0 {
		return "", "", fmt.Errorf("git bundle list-heads produced no heads")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	st, _ := exec.CommandContext(ctx, "git", "-C", cwd, "status", "--porcelain").Output()
	if len(bytes.TrimSpace(st)) > 0 {
		warning = "uncommitted changes were not included in the review bundle"
	}
	fileID, err = c.uploadFile(ctx, filepath.Base(path), data)
	return fileID, warning, err
}

// uploadFile mirrors zor()/N7t: POST {base}/v1/files multipart with a
// file part and purpose=user_data; response {id}.
func (c *Client) uploadFile(ctx context.Context, filename string, data []byte) (string, error) {
	tok, err := c.oauthToken()
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := w.WriteField("purpose", "user_data"); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	h, err := c.codeSessionHeaders(tok)
	if err != nil {
		return "", err
	}
	h.Set("Content-Type", w.FormDataContentType())
	// raw multipart body — c.do is the JSON lane; build the request by hand.
	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		c.BaseURL+"/v1/files", &body)
	if err != nil {
		return "", err
	}
	for k, vs := range h {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rbody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return "", fmt.Errorf("file upload failed %d: %s", resp.StatusCode, bodyErrorMessage(rbody))
	}
	r := &response{Status: resp.StatusCode, Body: rbody}
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(r.Body, &v); err != nil || v.ID == "" {
		return "", fmt.Errorf("upload succeeded but no file ID returned")
	}
	return v.ID, nil
}
