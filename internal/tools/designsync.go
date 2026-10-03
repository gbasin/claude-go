package tools

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DesignSync (mod_1599.js, mod_1797.js) — client for the claude.ai design
// service. Real behavior: every method except report_validate resolves a
// design-capable claude.ai credential (design scopes on the login, or a
// separate /design login credential), then calls the design RPC. Writes go
// through a two-phase boundary: finalize_plan mints a local plan token and
// write/delete/register/unregister refuse paths outside it. Consent bits
// (agent_design_projects) are server-granted via /v1/design/consent; a 403
// needs_consent surfaces as the "run /design consent" error.
//
// The exact REST paths for the project RPC live in chunks not present in
// the extraction; the calls below implement the documented resource shape
// (/v1/design/projects…, consent/grants confirmed in mod_1034.js) and pass
// server responses through verbatim.

const designAPIBase = "https://api.claude.ai"

func init() {
	Register(&Tool{Def: loadSchema("DesignSync"), Exec: execDesignSync})
}

// ---------- input ----------

type dsFile struct {
	Path      string `json:"path"`
	LocalPath string `json:"localPath"`
	Data      string `json:"data"`
	Encoding  string `json:"encoding"`
	MimeType  string `json:"mimeType"`
}

type dsAsset struct {
	Name     string          `json:"name"`
	Path     string          `json:"path"`
	Subtitle string          `json:"subtitle"`
	Viewport json.RawMessage `json:"viewport"`
	Group    string          `json:"group"`
}

type dsIn struct {
	Method    string    `json:"method"`
	ProjectID string    `json:"projectId"`
	Path      string    `json:"path"`
	Writes    []string  `json:"writes"`
	Deletes   []string  `json:"deletes"`
	PlanID    string    `json:"planId"`
	Files     []dsFile  `json:"files"`
	Paths     []string  `json:"paths"`
	Name      string    `json:"name"`
	Assets    []dsAsset `json:"assets"`
	LocalDir  string    `json:"localDir"`
	Counts    any       `json:"counts"`
}

// ---------- plan-token lifecycle ----------
//
// finalize_plan is the write boundary: it mints a token binding {projectId,
// writes, deletes, localDir}. Tokenless design writes are native-tool-only —
// write_files/delete_files/register_assets/unregister_assets without a
// matching plan token are refused.

type dsPlan struct {
	ProjectID string
	Writes    []string
	Deletes   []string
	LocalDir  string
}

var (
	dsPlanMu sync.Mutex
	dsPlans  = map[string]dsPlan{}
)

func dsNewPlan(p dsPlan) string {
	var b [16]byte
	rand.Read(b[:])
	tok := "plan_" + hex.EncodeToString(b[:])
	dsPlanMu.Lock()
	dsPlans[tok] = p
	dsPlanMu.Unlock()
	return tok
}

func dsGetPlan(tok string) (dsPlan, bool) {
	dsPlanMu.Lock()
	defer dsPlanMu.Unlock()
	p, ok := dsPlans[tok]
	return p, ok
}

// ---------- path helpers ----------

// dsNormPath is PP(): normalize a project-relative path.
func dsNormPath(p string) string {
	p = filepath.ToSlash(p)
	p = path.Clean(p)
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	return p
}

// dsReserved is von(): CLAUDE.md and .claude/ carry instructions to the
// design agent and are blocked regardless of the plan.
func dsReserved(p string) bool {
	p = dsNormPath(p)
	return p == "CLAUDE.md" || strings.HasSuffix(p, "/CLAUDE.md") ||
		p == ".claude" || strings.HasPrefix(p, ".claude/") ||
		strings.Contains(p, "/.claude/")
}

// dsMatchGlob is Con(): `*` matches within one segment, `**` any depth.
func dsMatchGlob(pattern, p string) bool {
	return dsMatchSeg(strings.Split(dsNormPath(pattern), "/"), strings.Split(dsNormPath(p), "/"))
}

func dsMatchSeg(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if dsMatchSeg(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	return dsMatchOne(pat[0], segs[0]) && dsMatchSeg(pat[1:], segs[1:])
}

func dsMatchOne(pat, seg string) bool {
	// single-segment wildcard match
	if pat == "*" {
		return true
	}
	if !strings.Contains(pat, "*") {
		return pat == seg
	}
	parts := strings.Split(pat, "*")
	if !strings.HasPrefix(seg, parts[0]) {
		return false
	}
	seg = seg[len(parts[0]):]
	for i := 1; i < len(parts); i++ {
		piece := parts[i]
		if piece == "" {
			continue
		}
		if i == len(parts)-1 {
			return strings.HasSuffix(seg, piece)
		}
		j := strings.Index(seg, piece)
		if j < 0 {
			return false
		}
		seg = seg[j+len(piece):]
	}
	return true
}

func dsInPlan(p string, plan []string) bool {
	for _, pat := range plan {
		if dsMatchGlob(pat, p) {
			return true
		}
	}
	return false
}

// dsTextExt is Se: extensions uploaded as UTF-8; anything else is base64.
var dsTextExt = map[string]bool{
	"html": true, "css": true, "js": true, "jsx": true, "mjs": true,
	"cjs": true, "ts": true, "tsx": true, "mts": true, "cts": true,
	"json": true, "svg": true, "xml": true, "md": true, "txt": true,
	"csv": true, "yaml": true, "yml": true, "toml": true,
}

const dsMaxFileBytes = 12582912 // te in mod_1599

// dsReadLocal mirrors Pe(): read a localPath file bounded to the plan's
// approved localDir, realpath-checked, regular file, size cap.
func dsReadLocal(f dsFile, planDir string) (data, encoding string, err error) {
	if planDir == "" {
		return "", "", fmt.Errorf("write_files with localPath requires a plan finalized with localDir. Re-run finalize_plan with the bundle directory.")
	}
	clean := func(p string) string {
		if !strings.HasSuffix(p, "/") {
			return p + "/"
		}
		return p
	}
	abs, err := filepath.Abs(filepath.Join(planDir, f.LocalPath))
	if err != nil {
		return "", "", err
	}
	if abs != planDir && !strings.HasPrefix(abs, clean(planDir)) {
		return "", "", fmt.Errorf("write_files: localPath must be inside the directory approved at finalize_plan.")
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", err
	}
	realDir, err := filepath.EvalSymlinks(planDir)
	if err != nil {
		return "", "", err
	}
	if real != realDir && !strings.HasPrefix(real, clean(realDir)) {
		return "", "", fmt.Errorf("write_files: localPath resolves outside the directory approved at finalize_plan.")
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", "", err
	}
	if !fi.Mode().IsRegular() {
		return "", "", fmt.Errorf("write_files: localPath must be a regular file.")
	}
	if fi.Size() > dsMaxFileBytes {
		return "", "", fmt.Errorf("write_files: file at localPath exceeds the %d byte limit.", dsMaxFileBytes)
	}
	b, err := os.ReadFile(real)
	if err != nil {
		return "", "", err
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(real)), ".")
	if dsTextExt[ext] {
		return string(b), "", nil
	}
	return b64enc(b), "base64", nil
}

func b64enc(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// ---------- consent / scopes ----------

// dsRequiredScope: read methods need user:design:read, everything that can
// mutate needs user:design:write.
func dsRequiredScope(method string) string {
	switch method {
	case "list_projects", "get_project", "list_files", "get_file", "report_validate":
		return "user:design:read"
	default:
		return "user:design:write"
	}
}

// dsScopes reads the recorded OAuth scopes: ~/.claude.json
// oauthAccount.scopes first, then ~/.claude/.credentials.json
// claudeAiOauth.scopes. Returns nil when no scope list is on record (the
// call then goes out and the server's response is surfaced verbatim).
func dsScopes() []string {
	home, _ := os.UserHomeDir()
	var out []string
	if b, err := os.ReadFile(filepath.Join(home, ".claude.json")); err == nil {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			if acct, ok := m["oauthAccount"].(map[string]any); ok {
				out = append(out, dsScopeList(acct["scopes"])...)
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json")); err == nil {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			if o, ok := m["claudeAiOauth"].(map[string]any); ok {
				out = append(out, dsScopeList(o["scopes"])...)
			}
		}
	}
	return out
}

func dsScopeList(v any) []string {
	switch t := v.(type) {
	case string:
		return strings.Fields(t)
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ---------- HTTP ----------

func dsCall(c *Context, method, path string, body any, out any) Result {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, designAPIBase+path, rdr)
	if err != nil {
		return Result{Content: err.Error(), IsError: true}
	}
	req.Header.Set("Authorization", "Bearer "+c.OAuthToken())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	cl := &http.Client{Timeout: 60 * time.Second}
	resp, err := cl.Do(req.WithContext(c.Ctx))
	if err != nil {
		return Result{Content: "Claude Design unavailable: " + err.Error(), IsError: true}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	// error envelope checks the real client performs first
	var env struct {
		Error   string `json:"error"`
		Consent string `json:"consent"`
	}
	json.Unmarshal(b, &env)
	switch {
	case resp.StatusCode == 401 || env.Error == "insufficient_scope":
		return Result{Content: "No design-capable credential. Run /design login to authorize Claude Design.", IsError: true}
	case resp.StatusCode == 403 && env.Error == "needs_consent":
		bit := env.Consent
		if bit == "" {
			bit = "agent_design_projects"
		}
		return Result{Content: fmt.Sprintf(
			"Connect to Claude Design? Claude can read and edit your Design projects from this tool. "+
				"Change anytime at claude.ai/design/settings or with /design revoke. "+
				"The user hasn't granted %s — run /design consent to grant it.", bit),
			IsError: true}
	case resp.StatusCode == 403 && env.Error == "needs_design_scopes":
		return Result{Content: "Claude Design rejected this session's claude.ai credential (HTTP 403): " +
			"it does not carry Claude Design access. Run /design login to authorize Claude Design, then retry.",
			IsError: true}
	case resp.StatusCode == 403 && env.Error == "needs_project_grant":
		return Result{Content: "Claude Design requires a project write grant for this project: " +
			truncStr(string(b), 500), IsError: true}
	case resp.StatusCode >= 400:
		return Result{Content: fmt.Sprintf("Claude Design %s failed: HTTP %s — %s",
			method, resp.Status, truncStr(string(b), 1000)), IsError: true}
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return Result{Content: truncStr(string(b), 100000)}
		}
		return Result{Content: ""}
	}
	var v any
	if json.Unmarshal(b, &v) == nil {
		pb, _ := json.MarshalIndent(v, "", "  ")
		return Result{Content: string(pb)}
	}
	return Result{Content: truncStr(string(b), 100000)}
}

func dsGet(c *Context, path string, out any) Result {
	return dsCall(c, "GET", path, nil, out)
}

// ---------- tool ----------

func execDesignSync(c *Context, raw json.RawMessage) Result {
	var in dsIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if in.Method == "report_validate" {
		return Result{Content: `{"method":"report_validate"}`}
	}
	if in.Method == "" {
		return Result{Content: "method is required", IsError: true}
	}
	// credential: OAuth token, never an API key
	if c.OAuthToken == nil || c.OAuthToken() == "" {
		return Result{Content: "No design-capable credential. Run /design login to authorize Claude Design.", IsError: true}
	}
	// consent gate: the login must carry the design scopes on record. When no
	// scope list is recorded locally the call still goes out and the
	// server's own consent/scope errors are surfaced verbatim.
	scopes := dsScopes()
	if scopes != nil {
		need := dsRequiredScope(in.Method)
		found := false
		for _, s := range scopes {
			if s == need {
				found = true
				break
			}
		}
		if !found {
			return Result{Content: fmt.Sprintf(
				"Your claude.ai login does not carry Claude Design access (missing scope %s). "+
					"The user hasn't granted this — run /design consent to grant it "+
					"(change anytime at claude.ai/design/settings or with /design revoke).", need),
				IsError: true}
		}
	}
	switch in.Method {
	case "list_projects":
		var res struct {
			Items []struct {
				ProjectID        string `json:"projectId"`
				Name             string `json:"name"`
				OwnerDisplayName string `json:"ownerDisplayName"`
				IsOwned          bool   `json:"isOwned"`
				UpdatedAt        string `json:"updatedAt"`
				CallerCanEdit    *bool  `json:"callerCanEdit"`
				CanEdit          *bool  `json:"canEdit"`
				Sharing          *struct {
					TeamCanEdit bool `json:"teamCanEdit"`
				} `json:"sharing"`
			} `json:"items"`
		}
		if r := dsGet(c, "/v1/design/projects?type=DESIGN_SYSTEM", &res); r.Content != "" || r.IsError {
			return r
		}
		out := map[string]any{"method": "list_projects"}
		var projects []map[string]any
		for _, p := range res.Items {
			canEdit := p.IsOwned ||
				(p.CallerCanEdit != nil && *p.CallerCanEdit) ||
				(p.CanEdit != nil && *p.CanEdit) ||
				(p.Sharing != nil && p.Sharing.TeamCanEdit)
			if !canEdit {
				continue // writable projects only
			}
			projects = append(projects, map[string]any{
				"projectId": p.ProjectID, "name": p.Name,
				"ownerDisplayName": p.OwnerDisplayName,
				"isOwned":          p.IsOwned, "updatedAt": p.UpdatedAt,
			})
		}
		out["projects"] = projects
		b, _ := json.MarshalIndent(out, "", "  ")
		return Result{Content: string(b)}

	case "get_project":
		if in.ProjectID == "" {
			return Result{Content: `get_project requires "projectId"`, IsError: true}
		}
		var res map[string]any
		if r := dsGet(c, "/v1/design/projects/"+url.PathEscape(in.ProjectID), &res); r.Content != "" || r.IsError {
			return r
		}
		res["method"] = "get_project"
		b, _ := json.MarshalIndent(res, "", "  ")
		return Result{Content: string(b)}

	case "list_files":
		if in.ProjectID == "" {
			return Result{Content: `list_files requires "projectId"`, IsError: true}
		}
		var res struct {
			Paths []string `json:"paths"`
			Files []string `json:"files"`
		}
		if r := dsGet(c, "/v1/design/projects/"+url.PathEscape(in.ProjectID)+"/files", &res); r.Content != "" || r.IsError {
			return r
		}
		paths := res.Paths
		if paths == nil {
			paths = res.Files
		}
		b, _ := json.MarshalIndent(map[string]any{"method": "list_files", "paths": paths}, "", "  ")
		return Result{Content: string(b)}

	case "get_file":
		if in.ProjectID == "" {
			return Result{Content: `get_file requires "projectId"`, IsError: true}
		}
		if in.Path == "" {
			return Result{Content: `get_file requires "path"`, IsError: true}
		}
		var res map[string]any
		p := "/v1/design/projects/" + url.PathEscape(in.ProjectID) +
			"/files/" + dsEscapePath(in.Path)
		if r := dsGet(c, p, &res); r.Content != "" || r.IsError {
			return r
		}
		res["method"] = "get_file"
		res["path"] = dsNormPath(in.Path)
		b, _ := json.MarshalIndent(res, "", "  ")
		return Result{Content: string(b)}

	case "finalize_plan":
		if in.ProjectID == "" {
			return Result{Content: `finalize_plan requires "projectId"`, IsError: true}
		}
		if len(in.Writes) == 0 && len(in.Deletes) == 0 {
			return Result{Content: "finalize_plan needs at least one write or delete path.", IsError: true}
		}
		dir := in.LocalDir
		if dir == "" {
			dir = c.CWD
		} else if !filepath.IsAbs(dir) {
			dir = filepath.Join(c.CWD, dir)
		}
		abs, err := filepath.Abs(dir)
		if err == nil {
			if real, e2 := filepath.EvalSymlinks(abs); e2 == nil {
				abs = real
			}
		}
		if err != nil {
			return Result{Content: fmt.Sprintf("localDir does not exist or is not accessible: %s (%s)",
				in.LocalDir, err), IsError: true}
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			e := "not a directory"
			if err != nil {
				e = err.Error()
			}
			return Result{Content: fmt.Sprintf("localDir does not exist or is not accessible: %s (%s)",
				in.LocalDir, e), IsError: true}
		}
		writes := make([]string, 0, len(in.Writes))
		for _, w := range in.Writes {
			writes = append(writes, dsNormPath(w))
		}
		deletes := make([]string, 0, len(in.Deletes))
		for _, d := range in.Deletes {
			deletes = append(deletes, dsNormPath(d))
		}
		planID := dsNewPlan(dsPlan{ProjectID: in.ProjectID, Writes: writes, Deletes: deletes, LocalDir: abs})
		b, _ := json.MarshalIndent(map[string]any{
			"method": "finalize_plan", "planId": planID,
			"writes": writes, "deletes": deletes,
		}, "", "  ")
		return Result{Content: string(b)}

	case "write_files":
		if in.ProjectID == "" {
			return Result{Content: `write_files requires "projectId"`, IsError: true}
		}
		if in.PlanID == "" {
			return Result{Content: `write_files requires "planId"`, IsError: true}
		}
		if len(in.Files) == 0 {
			return Result{Content: `write_files requires "files"`, IsError: true}
		}
		plan, ok := dsGetPlan(in.PlanID)
		if !ok || plan.ProjectID != in.ProjectID {
			return Result{Content: "Plan token is missing or does not match this project. Call finalize_plan first.", IsError: true}
		}
		var reserved, outside []string
		for _, f := range in.Files {
			if dsReserved(f.Path) {
				reserved = append(reserved, f.Path)
			}
			if !dsInPlan(f.Path, plan.Writes) {
				outside = append(outside, dsNormPath(f.Path))
			}
		}
		if len(reserved) > 0 {
			return Result{Content: fmt.Sprintf("Cannot write reserved paths: %s. CLAUDE.md and .claude/ carry instructions to the design agent and are blocked regardless of the plan.",
				strings.Join(reserved, ", ")), IsError: true}
		}
		if len(outside) > 0 {
			return Result{Content: fmt.Sprintf("Cannot write paths outside the finalized plan: %s. Re-run finalize_plan with the full set.",
				strings.Join(outside, ", ")), IsError: true}
		}
		// build upload entries (32 at a time in real; sequential is fine here)
		uploads := make([]map[string]any, 0, len(in.Files))
		for _, f := range in.Files {
			hasData := f.Data != ""
			hasLocal := f.LocalPath != ""
			if hasData == hasLocal {
				return Result{Content: fmt.Sprintf("Each file needs exactly one of \"data\" or \"localPath\" (offending path: %s).", f.Path), IsError: true}
			}
			if hasLocal && f.Encoding != "" {
				return Result{Content: fmt.Sprintf("\"encoding\" only applies to inline \"data\"; localPath files are encoded automatically (offending path: %s).", f.Path), IsError: true}
			}
			u := map[string]any{"path": dsNormPath(f.Path)}
			if f.MimeType != "" {
				u["mimeType"] = f.MimeType
			}
			if hasData {
				u["data"] = f.Data
				if f.Encoding != "" {
					u["encoding"] = f.Encoding
				}
			} else {
				data, enc, err := dsReadLocal(f, plan.LocalDir)
				if err != nil {
					return Result{Content: err.Error(), IsError: true}
				}
				u["data"] = data
				if enc != "" {
					u["encoding"] = enc
				}
			}
			uploads = append(uploads, u)
		}
		var res map[string]any
		r := dsCall(c, "POST", "/v1/design/projects/"+url.PathEscape(in.ProjectID)+"/files",
			map[string]any{"files": uploads}, &res)
		if r.Content != "" || r.IsError {
			return r
		}
		written := len(uploads)
		if w, ok := res["written"]; ok {
			if n, ok2 := w.(float64); ok2 {
				written = int(n)
			}
		}
		b, _ := json.MarshalIndent(map[string]any{"method": "write_files", "written": written}, "", "  ")
		return Result{Content: string(b)}

	case "delete_files":
		if in.ProjectID == "" {
			return Result{Content: `delete_files requires "projectId"`, IsError: true}
		}
		if in.PlanID == "" {
			return Result{Content: `delete_files requires "planId"`, IsError: true}
		}
		if len(in.Paths) == 0 {
			return Result{Content: `delete_files requires "paths"`, IsError: true}
		}
		plan, ok := dsGetPlan(in.PlanID)
		if !ok || plan.ProjectID != in.ProjectID {
			return Result{Content: "Plan token is missing or does not match this project. Call finalize_plan first.", IsError: true}
		}
		var reserved, outside []string
		for _, p := range in.Paths {
			if dsReserved(p) {
				reserved = append(reserved, p)
			}
			if !dsInPlan(p, plan.Deletes) {
				outside = append(outside, dsNormPath(p))
			}
		}
		if len(reserved) > 0 {
			return Result{Content: fmt.Sprintf("Cannot delete reserved paths: %s. CLAUDE.md and .claude/ carry instructions to the design agent and are blocked regardless of the plan.",
				strings.Join(reserved, ", ")), IsError: true}
		}
		if len(outside) > 0 {
			return Result{Content: fmt.Sprintf("Cannot delete paths outside the finalized plan: %s. Re-run finalize_plan with the full set.",
				strings.Join(outside, ", ")), IsError: true}
		}
		norm := make([]string, 0, len(in.Paths))
		for _, p := range in.Paths {
			norm = append(norm, dsNormPath(p))
		}
		var res map[string]any
		r := dsCall(c, "DELETE", "/v1/design/projects/"+url.PathEscape(in.ProjectID)+"/files",
			map[string]any{"paths": norm}, &res)
		if r.Content != "" || r.IsError {
			return r
		}
		deleted := len(norm)
		if d, ok := res["deleted"]; ok {
			if n, ok2 := d.(float64); ok2 {
				deleted = int(n)
			}
		}
		b, _ := json.MarshalIndent(map[string]any{"method": "delete_files", "deleted": deleted}, "", "  ")
		return Result{Content: string(b)}

	case "register_assets":
		if in.ProjectID == "" {
			return Result{Content: `register_assets requires "projectId"`, IsError: true}
		}
		if in.PlanID == "" {
			return Result{Content: `register_assets requires "planId"`, IsError: true}
		}
		if len(in.Assets) == 0 {
			return Result{Content: `register_assets requires "assets"`, IsError: true}
		}
		plan, ok := dsGetPlan(in.PlanID)
		if !ok || plan.ProjectID != in.ProjectID {
			return Result{Content: "Plan token is missing or does not match this project. Call finalize_plan first.", IsError: true}
		}
		var outside []string
		for _, a := range in.Assets {
			if !dsInPlan(a.Path, plan.Writes) {
				outside = append(outside, dsNormPath(a.Path))
			}
		}
		if len(outside) > 0 {
			return Result{Content: fmt.Sprintf("Cannot register paths outside the finalized plan: %s. Re-run finalize_plan with the full set.",
				strings.Join(outside, ", ")), IsError: true}
		}
		registered := 0
		for _, a := range in.Assets {
			body := map[string]any{
				"name": a.Name, "path": dsNormPath(a.Path),
			}
			if a.Subtitle != "" {
				body["subtitle"] = a.Subtitle
			}
			if a.Group != "" {
				body["group"] = a.Group
			}
			if len(a.Viewport) > 0 {
				var v any
				if json.Unmarshal(a.Viewport, &v) == nil {
					body["viewport"] = v
				}
			}
			if r := dsCall(c, "POST", "/v1/design/projects/"+url.PathEscape(in.ProjectID)+"/assets",
				body, nil); r.IsError {
				return r
			}
			registered++
		}
		b, _ := json.MarshalIndent(map[string]any{"method": "register_assets", "registered": registered}, "", "  ")
		return Result{Content: string(b)}

	case "unregister_assets":
		if in.ProjectID == "" {
			return Result{Content: `unregister_assets requires "projectId"`, IsError: true}
		}
		if in.PlanID == "" {
			return Result{Content: `unregister_assets requires "planId"`, IsError: true}
		}
		if len(in.Paths) == 0 {
			return Result{Content: `unregister_assets requires "paths"`, IsError: true}
		}
		plan, ok := dsGetPlan(in.PlanID)
		if !ok || plan.ProjectID != in.ProjectID {
			return Result{Content: "Plan token is missing or does not match this project. Call finalize_plan first.", IsError: true}
		}
		var outside []string
		for _, p := range in.Paths {
			if !dsInPlan(p, plan.Deletes) {
				outside = append(outside, dsNormPath(p))
			}
		}
		if len(outside) > 0 {
			return Result{Content: fmt.Sprintf("Cannot unregister cards for paths outside the finalized plan's deletes: %s. Re-run finalize_plan with the full set.",
				strings.Join(outside, ", ")), IsError: true}
		}
		unregistered := 0
		for _, p := range in.Paths {
			if r := dsCall(c, "DELETE", "/v1/design/projects/"+url.PathEscape(in.ProjectID)+
				"/assets/"+dsEscapePath(p), nil, nil); r.IsError {
				return r
			}
			unregistered++
		}
		b, _ := json.MarshalIndent(map[string]any{"method": "unregister_assets", "unregistered": unregistered}, "", "  ")
		return Result{Content: string(b)}

	case "create_project":
		if in.Name == "" {
			return Result{Content: `create_project requires "name"`, IsError: true}
		}
		var res map[string]any
		r := dsCall(c, "POST", "/v1/design/projects",
			map[string]any{"name": in.Name, "type": "PROJECT_TYPE_DESIGN_SYSTEM"}, &res)
		if r.Content != "" || r.IsError {
			return r
		}
		res["method"] = "create_project"
		b, _ := json.MarshalIndent(res, "", "  ")
		return Result{Content: string(b)}

	default:
		return Result{Content: "unknown method: " + in.Method, IsError: true}
	}
}

// dsEscapePath escapes a project-relative path for use as a URL suffix,
// preserving / separators.
func dsEscapePath(p string) string {
	segs := strings.Split(dsNormPath(p), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}
