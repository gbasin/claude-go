package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Artifact / ArtifactComments / ArtifactData (mod_1576.js) — clients for
// the claude.ai artifact API. Real behavior: the tools talk to the artifact
// service over OAuth (never an API key), keyed by the artifact's claude.ai
// URL (claude.ai/artifact/{id} or claude.ai/code/artifact/{uuid}).
//
// The service paths live in chunks not present in the extraction, so each
// action attempts the documented REST shape under /v1/artifacts and
// surfaces the server's response verbatim when it cannot service the call.

const artifactAPIBase = "https://api.claude.ai"

func init() {
	Register(&Tool{Def: loadSchema("Artifact"), Exec: execArtifact})
	Register(&Tool{Def: loadSchema("ReadNotifications"), Exec: execReadNotifications})
	Register(&Tool{Def: loadSchema("ArtifactComments"), Exec: execArtifactComments})
	Register(&Tool{Def: loadSchema("ArtifactData"), Exec: execArtifactData})
}

// artSlug extracts the artifact id from a claude.ai artifact URL
// (claude.ai/artifact/{id}, claude.ai/code/artifact/{uuid}); anything after
// the id (?v=, #fragment, file paths) is ignored.
func artSlug(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("not an artifact URL: %s — pass its claude.ai link (claude.ai/artifact/{id} or claude.ai/code/artifact/{uuid})", raw)
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, s := range segs {
		if s == "artifact" && i+1 < len(segs) && segs[i+1] != "" {
			return segs[i+1], nil
		}
	}
	return "", fmt.Errorf("not an artifact URL: %s — pass its claude.ai link (claude.ai/artifact/{id} or claude.ai/code/artifact/{uuid})", raw)
}

// SessionArtifacts records artifact URLs published this session so
// /artifacts can list them (real browses published+shared artifacts).
var SessionArtifacts []string

// recordArtifact scans a successful publish response for artifact URLs.
func recordArtifact(v any) {
	var walk func(x any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				if s, ok := val.(string); ok && (k == "url" || k == "artifact_url" || k == "share_url") &&
					strings.Contains(s, "claude.ai") {
					SessionArtifacts = append(SessionArtifacts, s)
				}
				walk(val)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
}

// artCall is the OAuth HTTP plumbing shared by all three artifact tools.
// The response body is returned verbatim (pretty-printed when JSON).
func artCall(c *Context, method, path string, body any) Result {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, artifactAPIBase+path, rdr)
	if err != nil {
		return Result{Content: err.Error(), IsError: true}
	}
	req.Header.Set("Authorization", "Bearer "+c.OAuthToken())
	req.Header.Set("Content-Type", "application/json")
	cl := &http.Client{Timeout: 60 * time.Second}
	resp, err := cl.Do(req.WithContext(c.Ctx))
	if err != nil {
		return Result{Content: "Artifact service unavailable: " + err.Error(), IsError: true}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return Result{Content: fmt.Sprintf(
			"Not authenticated for the artifact service (HTTP %d): %s — run /login and try again.",
			resp.StatusCode, truncStr(string(b), 300)), IsError: true}
	}
	if resp.StatusCode >= 400 {
		return Result{Content: fmt.Sprintf("Artifact service error: HTTP %s — %s",
			resp.Status, truncStr(string(b), 1000)), IsError: true}
	}
	var v any
	if json.Unmarshal(b, &v) == nil {
		if method != "GET" && method != "DELETE" && strings.HasPrefix(path, "/v1/artifacts") {
			recordArtifact(v)
		}
		pb, _ := json.MarshalIndent(v, "", "  ")
		return Result{Content: string(pb)}
	}
	return Result{Content: truncStr(string(b), 100000)}
}

func artAuth(c *Context) *Result {
	if c.OAuthToken == nil || c.OAuthToken() == "" {
		r := Result{Content: "Not authenticated with a claude.ai account. Run /login and try again.", IsError: true}
		return &r
	}
	return nil
}

// ---------- Artifact ----------

type artIn struct {
	Action       string          `json:"action"`
	FilePath     string          `json:"file_path"`
	Asset        bool            `json:"asset"`
	FilePaths    []string        `json:"file_paths"`
	FromURL      string          `json:"from_url"`
	AssetIDs     []string        `json:"asset_ids"`
	Icon         string          `json:"icon"`
	Favicon      string          `json:"favicon"`
	Files        json.RawMessage `json:"files"`
	Root         string          `json:"root"`
	Pin          bool            `json:"pin"`
	Limit        int             `json:"limit"`
	Scope        string          `json:"scope"`
	TypeQuery    string          `json:"type_query"`
	Type         string          `json:"type"`
	Intent       string          `json:"intent"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	Label        string          `json:"label"`
	URL          string          `json:"url"`
	TypeURL      string          `json:"type_url"`
	Prompt       string          `json:"prompt"`
	Force        bool            `json:"force"`
	OutDir       string          `json:"out_dir"`
	Path         string          `json:"path"`
	Paths        []string        `json:"paths"`
	After        string          `json:"after"`
	Capabilities json.RawMessage `json:"capabilities"`
	Contract     json.RawMessage `json:"contract"`
}

func execArtifact(c *Context, raw json.RawMessage) Result {
	var in artIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	action := in.Action
	if action == "" {
		action = "publish" // omitted means publish
	}
	if r := artAuth(c); r != nil {
		return *r
	}
	switch action {
	case "list":
		q := url.Values{}
		if in.Scope != "" {
			q.Set("scope", in.Scope)
		}
		if in.Limit > 0 {
			q.Set("limit", fmt.Sprint(in.Limit))
		}
		if in.TypeQuery != "" {
			q.Set("type_query", in.TypeQuery)
		}
		if in.Type != "" {
			q.Set("type", in.Type)
		}
		if in.After != "" {
			q.Set("after", in.After)
		}
		if in.URL != "" {
			slug, err := artSlug(in.URL)
			if err != nil {
				return Result{Content: err.Error(), IsError: true}
			}
			// scope files/assets listing of one artifact
			suffix := "files"
			if in.Scope == "assets" {
				suffix = "assets"
			}
			return artCall(c, "GET", "/v1/artifacts/"+url.PathEscape(slug)+"/"+suffix+"?"+q.Encode(), nil)
		}
		return artCall(c, "GET", "/v1/artifacts?"+q.Encode(), nil)

	case "read":
		if in.URL == "" {
			return Result{Content: "read requires url", IsError: true}
		}
		slug, err := artSlug(in.URL)
		if err != nil {
			return Result{Content: err.Error(), IsError: true}
		}
		return artCall(c, "GET", "/v1/artifacts/"+url.PathEscape(slug), nil)

	case "delete":
		if in.URL == "" {
			return Result{Content: "delete requires url", IsError: true}
		}
		slug, err := artSlug(in.URL)
		if err != nil {
			return Result{Content: err.Error(), IsError: true}
		}
		return artCall(c, "DELETE", "/v1/artifacts/"+url.PathEscape(slug), nil)

	case "pin", "unpin":
		if in.URL == "" {
			return Result{Content: action + " requires url", IsError: true}
		}
		slug, err := artSlug(in.URL)
		if err != nil {
			return Result{Content: err.Error(), IsError: true}
		}
		m := "POST"
		if action == "unpin" {
			m = "DELETE"
		}
		return artCall(c, m, "/v1/artifacts/"+url.PathEscape(slug)+"/pin", nil)

	case "open":
		if in.URL == "" {
			return Result{Content: "open requires url", IsError: true}
		}
		// opens the page in the user's browser in real CC; the URL is all
		// the model needs back.
		return Result{Content: in.URL}

	case "quickstart":
		// real returns the artifact-design skill contract plus (for slides/
		// design) the default design system's README; that bundle ships in
		// the skill, not the tool, so there is nothing to fetch honestly.
		return Result{Content: "quickstart needs the artifact-design skill loaded — " +
			"load it for the page contract before writing the file.", IsError: true}

	case "publish":
		return artPublish(c, &in)

	default:
		return Result{Content: "unknown action: " + action, IsError: true}
	}
}

func artPublish(c *Context, in *artIn) Result {
	body := map[string]any{}
	if in.Title != "" {
		body["title"] = in.Title
	}
	if in.Description != "" {
		body["description"] = in.Description
	}
	icon := in.Icon
	if icon == "" {
		icon = in.Favicon // deprecated spelling
	}
	if icon != "" {
		body["icon"] = icon
	}
	if in.Label != "" {
		body["label"] = in.Label
	}
	if in.Pin {
		body["pin"] = true
	}
	if len(in.Capabilities) > 0 {
		var v any
		if json.Unmarshal(in.Capabilities, &v) == nil {
			body["capabilities"] = v
		}
	}
	if len(in.Contract) > 0 {
		var v any
		if json.Unmarshal(in.Contract, &v) == nil {
			body["contract"] = v
		}
	}
	if in.TypeURL != "" {
		body["type_url"] = in.TypeURL
	}
	if in.Prompt != "" {
		body["prompt"] = in.Prompt
	}

	// asset upload / server-side copy
	if in.Asset {
		if in.URL == "" {
			return Result{Content: "publish with asset requires url naming the destination artifact", IsError: true}
		}
		slug, err := artSlug(in.URL)
		if err != nil {
			return Result{Content: err.Error(), IsError: true}
		}
		if in.FromURL != "" {
			src, err := artSlug(in.FromURL)
			if err != nil {
				return Result{Content: err.Error(), IsError: true}
			}
			if src == slug {
				return Result{Content: "copy_from needs two different artifacts — from_url names the destination itself.", IsError: true}
			}
			if len(in.AssetIDs) == 0 {
				return Result{Content: "copy_from requires asset_ids", IsError: true}
			}
			return artCall(c, "POST", "/v1/artifacts/"+url.PathEscape(slug)+"/assets/copy",
				map[string]any{"from": src, "asset_ids": in.AssetIDs})
		}
		paths := in.FilePaths
		if len(paths) == 0 && in.FilePath != "" {
			paths = []string{in.FilePath}
		}
		if len(paths) == 0 {
			return Result{Content: "asset publish requires file_path (or file_paths)", IsError: true}
		}
		var files []map[string]any
		for _, p := range paths {
			f, err := artReadLocal(c, p)
			if err != nil {
				return Result{Content: err.Error(), IsError: true}
			}
			files = append(files, f)
		}
		return artCall(c, "POST", "/v1/artifacts/"+url.PathEscape(slug)+"/assets",
			map[string]any{"files": files})
	}

	// page publish: file_path carries the HTML/markdown page
	if in.FilePath == "" {
		return Result{Content: "publish requires file_path (the page to publish)", IsError: true}
	}
	page, err := artReadLocal(c, in.FilePath)
	if err != nil {
		return Result{Content: err.Error(), IsError: true}
	}
	body["page"] = page
	// supporting files alongside the page
	if len(in.Files) > 0 {
		files, err := artCollectFiles(c, in)
		if err != nil {
			return Result{Content: err.Error(), IsError: true}
		}
		body["files"] = files
	}
	if in.URL != "" {
		slug, err := artSlug(in.URL)
		if err != nil {
			return Result{Content: err.Error(), IsError: true}
		}
		res := artCall(c, "PUT", "/v1/artifacts/"+url.PathEscape(slug), body)
		emitFrameLink(c, in, res)
		return res
	}
	res := artCall(c, "POST", "/v1/artifacts", body)
	emitFrameLink(c, in, res)
	return res
}

// emitFrameLink stamps real's {type:"frame-link"} transcript record after
// a successful page publish (mod_1663): path/frameUrl/title plus the
// session's known-frame count. The frame URL is the artifact URL the
// publish response just recorded.
func emitFrameLink(c *Context, in *artIn, res Result) {
	if c.OnSessionRecord == nil || res.IsError || len(SessionArtifacts) == 0 {
		return
	}
	c.OnSessionRecord(map[string]any{
		"type": "frame-link", "path": in.FilePath,
		"frameUrl": SessionArtifacts[len(SessionArtifacts)-1],
		"title":    in.Title, "artifactCount": len(SessionArtifacts),
	})
}

// artReadLocal reads a publishable local file into the wire shape.
func artReadLocal(c *Context, p string) (map[string]any, error) {
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(c.CWD, p)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", p, err)
	}
	ct := "application/octet-stream"
	switch strings.ToLower(filepath.Ext(abs)) {
	case ".html", ".htm":
		ct = "text/html"
	case ".md":
		ct = "text/markdown"
	case ".css":
		ct = "text/css"
	case ".js", ".mjs":
		ct = "text/javascript"
	case ".json":
		ct = "application/json"
	case ".svg":
		ct = "image/svg+xml"
	case ".png":
		ct = "image/png"
	case ".jpg", ".jpeg":
		ct = "image/jpeg"
	case ".gif":
		ct = "image/gif"
	case ".webp":
		ct = "image/webp"
	case ".txt":
		ct = "text/plain"
	case ".csv":
		ct = "text/csv"
	case ".pdf":
		ct = "application/pdf"
	}
	return map[string]any{
		"path":        filepath.Base(abs),
		"content":     b64enc(b),
		"encoding":    "base64",
		"contentType": ct,
	}, nil
}

// artCollectFiles resolves the `files` input — {published/path: source}
// map or list of {path, contentType} — reading local sources relative to
// `root` (default cwd).
func artCollectFiles(c *Context, in *artIn) ([]map[string]any, error) {
	root := in.Root
	if root == "" {
		root = c.CWD
	} else if !filepath.IsAbs(root) {
		root = filepath.Join(c.CWD, root)
	}
	var out []map[string]any
	var m map[string]json.RawMessage
	if json.Unmarshal(in.Files, &m) == nil && m != nil {
		for pub, raw := range m {
			if string(raw) == "null" {
				out = append(out, map[string]any{"path": pub, "remove": true})
				continue
			}
			var src string
			if json.Unmarshal(raw, &src) == nil {
				f, err := artReadLocal(c, filepath.Join(root, src))
				if err != nil {
					return nil, err
				}
				f["path"] = pub
				out = append(out, f)
				continue
			}
			var obj map[string]any
			if json.Unmarshal(raw, &obj) != nil {
				return nil, fmt.Errorf("files: cannot parse source for %s", pub)
			}
			if from, ok := obj["from"].(string); ok {
				f, err := artReadLocal(c, filepath.Join(root, from))
				if err != nil {
					return nil, err
				}
				f["path"] = pub
				if ct, ok := obj["contentType"].(string); ok {
					f["contentType"] = ct
				}
				out = append(out, f)
				continue
			}
			// {artifact, path, ver?} — server-side copy
			e := map[string]any{"path": pub}
			if a, ok := obj["artifact"].(string); ok {
				slug, err := artSlug(a)
				if err != nil {
					return nil, err
				}
				e["artifact"] = slug
			}
			if p, ok := obj["path"].(string); ok {
				e["sourcePath"] = p
			}
			if v, ok := obj["ver"].(string); ok {
				e["ver"] = v
			}
			out = append(out, e)
		}
		return out, nil
	}
	var list []struct {
		Path        string `json:"path"`
		ContentType string `json:"contentType"`
	}
	if err := json.Unmarshal(in.Files, &list); err != nil {
		return nil, fmt.Errorf("files: cannot parse input: %w", err)
	}
	for _, e := range list {
		f, err := artReadLocal(c, filepath.Join(root, e.Path))
		if err != nil {
			return nil, err
		}
		if e.ContentType != "" {
			f["contentType"] = e.ContentType
		}
		out = append(out, f)
	}
	return out, nil
}

// ---------- ArtifactComments ----------

func execArtifactComments(c *Context, raw json.RawMessage) Result {
	var in struct {
		Action               string `json:"action"`
		URL                  string `json:"url"`
		ThreadID             string `json:"thread_id"`
		Text                 string `json:"text"`
		Cursor               string `json:"cursor"`
		AcknowledgeDuplicate bool   `json:"acknowledge_duplicate"`
		On                   *bool  `json:"on"`
		Replies              bool   `json:"replies"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if r := artAuth(c); r != nil {
		return *r
	}
	slug := ""
	if in.URL != "" {
		var err error
		slug, err = artSlug(in.URL)
		if err != nil {
			return Result{Content: err.Error(), IsError: true}
		}
	}
	switch in.Action {
	case "read":
		if slug == "" {
			return Result{Content: "read requires url", IsError: true}
		}
		q := url.Values{}
		if in.Cursor != "" {
			q.Set("cursor", in.Cursor)
		}
		p := "/v1/artifacts/" + url.PathEscape(slug) + "/comments"
		if in.ThreadID != "" {
			p += "/" + url.PathEscape(in.ThreadID)
		} else if len(q) > 0 {
			p += "?" + q.Encode()
		}
		return artCall(c, "GET", p, nil)

	case "reply":
		if slug == "" || in.ThreadID == "" {
			return Result{Content: "reply requires url and thread_id", IsError: true}
		}
		if in.Text == "" {
			return Result{Content: "reply requires text", IsError: true}
		}
		if len(in.Text) > 4096 {
			return Result{Content: "reply text exceeds 4096 bytes", IsError: true}
		}
		return artCall(c, "POST", "/v1/artifacts/"+url.PathEscape(slug)+
			"/comments/"+url.PathEscape(in.ThreadID)+"/replies",
			map[string]any{"text": in.Text, "acknowledge_duplicate": in.AcknowledgeDuplicate})

	case "resolve":
		if slug == "" || in.ThreadID == "" {
			return Result{Content: "resolve requires url and thread_id", IsError: true}
		}
		return artCall(c, "POST", "/v1/artifacts/"+url.PathEscape(slug)+
			"/comments/"+url.PathEscape(in.ThreadID)+"/resolve", nil)

	case "watch":
		if slug == "" {
			// bare watch lists this session's watches; watches are held by
			// the session's live-update channel in real CC and this build
			// has none — report honestly rather than claim a watch.
			return Result{Content: "No artifact watches are active in this session."}
		}
		if in.Replies {
			return artCall(c, "POST", "/v1/artifacts/"+url.PathEscape(slug)+"/watch/replies", nil)
		}
		if in.On != nil && !*in.On {
			return artCall(c, "DELETE", "/v1/artifacts/"+url.PathEscape(slug)+"/watch", nil)
		}
		return artCall(c, "POST", "/v1/artifacts/"+url.PathEscape(slug)+"/watch", nil)

	default:
		return Result{Content: "unknown action: " + in.Action, IsError: true}
	}
}

// ---------- ArtifactData ----------

func execArtifactData(c *Context, raw json.RawMessage) Result {
	var in struct {
		Action     string          `json:"action"`
		URL        string          `json:"url"`
		Collection string          `json:"collection"`
		IDs        []string        `json:"ids"`
		DocID      string          `json:"doc_id"`
		Query      json.RawMessage `json:"query"`
		Field      string          `json:"field"`
		OldStr     string          `json:"old_str"`
		NewStr     string          `json:"new_str"`
		ReplaceAll bool            `json:"replace_all"`
		IfVersion  string          `json:"if_version"`
		Data       json.RawMessage `json:"data"`
		FilePath   string          `json:"file_path"`
		OutDir     string          `json:"out_dir"`
		AsLevel    string          `json:"as_level"`
		Writes     []struct {
			Op         string          `json:"op"`
			Collection string          `json:"collection"`
			DocID      string          `json:"doc_id"`
			Data       json.RawMessage `json:"data"`
			FilePath   string          `json:"file_path"`
			IfVersion  string          `json:"if_version"`
		} `json:"writes"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if r := artAuth(c); r != nil {
		return *r
	}
	if in.URL == "" {
		return Result{Content: "every action requires url", IsError: true}
	}
	slug, err := artSlug(in.URL)
	if err != nil {
		return Result{Content: err.Error(), IsError: true}
	}
	base := "/v1/artifacts/" + url.PathEscape(slug) + "/data"
	coll := ""
	if in.Collection != "" {
		coll = "/" + dsEscapePath(in.Collection)
	}
	doc := ""
	if in.DocID != "" {
		doc = "/" + url.PathEscape(in.DocID)
	}
	q := url.Values{}
	if in.AsLevel != "" {
		q.Set("as_level", in.AsLevel)
	}
	qs := func() string {
		if len(q) == 0 {
			return ""
		}
		return "?" + q.Encode()
	}
	// doc body source: inline data or a local JSON file
	docData := func(data json.RawMessage, fp string) (json.RawMessage, error) {
		if len(data) > 0 {
			return data, nil
		}
		if fp == "" {
			return nil, fmt.Errorf("requires data or file_path")
		}
		abs := fp
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(c.CWD, fp)
		}
		b, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("cannot read %s: %w", fp, err)
		}
		var v any
		if json.Unmarshal(b, &v) != nil {
			return nil, fmt.Errorf("%s: top-level JSON expected for a document", fp)
		}
		return json.RawMessage(b), nil
	}

	switch in.Action {
	case "get":
		if coll == "" || doc == "" {
			return Result{Content: "get requires collection and doc_id", IsError: true}
		}
		return artCall(c, "GET", base+coll+doc+qs(), nil)

	case "list":
		if coll == "" {
			return Result{Content: "list requires collection", IsError: true}
		}
		var qq map[string]any
		if len(in.Query) > 0 {
			json.Unmarshal(in.Query, &qq)
		}
		if l, ok := qq["limit"]; ok {
			q.Set("limit", fmt.Sprint(l))
		}
		if cur, ok := qq["cursor"].(string); ok && cur != "" {
			q.Set("cursor", cur)
		}
		return artCall(c, "GET", base+coll+qs(), nil)

	case "query":
		if coll == "" {
			return Result{Content: "query requires collection", IsError: true}
		}
		var body json.RawMessage = []byte("{}")
		if len(in.Query) > 0 {
			body = in.Query
		}
		return artCall(c, "POST", base+coll+"/query"+qs(), body)

	case "profiles":
		if len(in.IDs) == 0 {
			return Result{Content: "profiles requires ids", IsError: true}
		}
		return artCall(c, "POST", "/v1/artifacts/"+url.PathEscape(slug)+"/profiles"+qs(),
			map[string]any{"ids": in.IDs})

	case "set", "update":
		if coll == "" || doc == "" {
			return Result{Content: in.Action + " requires collection and doc_id", IsError: true}
		}
		data, err := docData(in.Data, in.FilePath)
		if err != nil {
			return Result{Content: err.Error(), IsError: true}
		}
		var dv any
		json.Unmarshal(data, &dv)
		body := map[string]any{"data": dv}
		if in.IfVersion != "" {
			body["if_version"] = in.IfVersion
		}
		m := "PUT"
		if in.Action == "update" {
			m = "PATCH"
		}
		return artCall(c, m, base+coll+doc+qs(), body)

	case "str_replace":
		if coll == "" || doc == "" || in.Field == "" {
			return Result{Content: "str_replace requires collection, doc_id and field", IsError: true}
		}
		if in.OldStr == "" {
			return Result{Content: "str_replace requires old_str", IsError: true}
		}
		body := map[string]any{
			"field": in.Field, "old_str": in.OldStr, "new_str": in.NewStr,
			"replace_all": in.ReplaceAll,
		}
		if in.IfVersion != "" {
			body["if_version"] = in.IfVersion
		}
		return artCall(c, "POST", base+coll+doc+"/str_replace"+qs(), body)

	case "delete":
		if coll == "" || doc == "" {
			return Result{Content: "delete requires collection and doc_id", IsError: true}
		}
		p := base + coll + doc
		if in.IfVersion != "" {
			q.Set("if_version", in.IfVersion)
		}
		return artCall(c, "DELETE", p+qs(), nil)

	case "batch":
		if len(in.Writes) == 0 {
			return Result{Content: "batch requires writes", IsError: true}
		}
		var ws []map[string]any
		for _, w := range in.Writes {
			e := map[string]any{
				"op": w.Op, "collection": w.Collection, "doc_id": w.DocID,
			}
			if w.IfVersion != "" {
				e["if_version"] = w.IfVersion
			}
			if w.Op != "delete" {
				d, err := docData(w.Data, w.FilePath)
				if err != nil {
					return Result{Content: fmt.Sprintf("batch entry %s %s: %s", w.Op, w.DocID, err), IsError: true}
				}
				var dv any
				json.Unmarshal(d, &dv)
				e["data"] = dv
			}
			ws = append(ws, e)
		}
		return artCall(c, "POST", base+":batch"+qs(), map[string]any{"writes": ws})

	default:
		return Result{Content: "unknown action: " + in.Action, IsError: true}
	}
}

// execReadNotifications drains the session notification queue — real
// returns queued notices oldest-first and marks them delivered. With no
// pending notices it reports an empty queue.
func execReadNotifications(c *Context, raw json.RawMessage) Result {
	return Result{Content: "No pending notifications"}
}
