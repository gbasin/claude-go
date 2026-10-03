package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

func init() {
	Register(&Tool{Def: loadSchema("WebFetch"), Exec: execWebFetch})
	Register(&Tool{Def: loadSchema("WebSearch"), Exec: execWebSearch})
}

type fetchIn struct {
	URL    string `json:"url"`
	Prompt string `json:"prompt"`
}

var tagRe = regexp.MustCompile(`<[^>]+>`)
var scriptStyleRe = regexp.MustCompile(`(?is)<(script|style|noscript)[^>]*>.*?</(script|style|noscript)>`)
var wsRe = regexp.MustCompile(`[ \t]+`)
var blankRe = regexp.MustCompile(`\n{3,}`)

func htmlToText(b []byte) string {
	s := scriptStyleRe.ReplaceAllString(string(b), " ")
	// line-break-ish tags
	for _, t := range []string{"p", "div", "br", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6", "section", "article", "header", "footer"} {
		s = strings.ReplaceAll(s, "<"+t, "\n<"+t)
		s = strings.ReplaceAll(s, "</"+t+">", "\n")
	}
	s = tagRe.ReplaceAllString(s, "")
	s = wsRe.ReplaceAllString(s, " ")
	s = blankRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// fetchCache is the 15-minute per-URL cache real WebFetch uses.
var fetchCache = struct {
	mu   sync.Mutex
	data map[string]struct {
		at  time.Time
		res Result
	}
}{data: map[string]struct {
	at  time.Time
	res Result
}{}}

func execWebFetch(c *Context, raw json.RawMessage) Result {
	var in fetchIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	key := in.URL + "|" + in.Prompt
	fetchCache.mu.Lock()
	if hit, ok := fetchCache.data[key]; ok && time.Since(hit.at) < 15*time.Minute {
		fetchCache.mu.Unlock()
		return hit.res
	}
	// evict expired
	for k, v := range fetchCache.data {
		if time.Since(v.at) >= 15*time.Minute {
			delete(fetchCache.data, k)
		}
	}
	fetchCache.mu.Unlock()
	res := doWebFetch(c, in)
	fetchCache.mu.Lock()
	fetchCache.data[key] = struct {
		at  time.Time
		res Result
	}{time.Now(), res}
	fetchCache.mu.Unlock()
	return res
}

func doWebFetch(c *Context, in fetchIn) Result {
	u, err := url.Parse(in.URL)
	if err != nil || u.Host == "" {
		return Result{Content: "Invalid URL: " + in.URL, IsError: true}
	}
	if u.Scheme == "http" {
		u.Scheme = "https" // per spec: HTTP upgraded to HTTPS
	}
	if !strings.Contains(u.Hostname(), ".") {
		return Result{Content: "Cannot fetch localhost or hostnames without a dot; use curl via Bash for local servers.", IsError: true}
	}
	ctx, cancel := context.WithTimeout(c.Ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	req.Header.Set("User-Agent", "claude-go/"+version()+" (WebFetch)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Result{Content: "Fetch failed: " + err.Error(), IsError: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := resp.Header.Get("Location")
		return Result{Content: fmt.Sprintf("Redirect (not followed): %s — call again with this URL.", loc), IsError: true}
	}
	if resp.StatusCode != 200 {
		return Result{Content: fmt.Sprintf("Fetch failed with status %d", resp.StatusCode), IsError: true}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return Result{Content: "Fetch failed: " + err.Error(), IsError: true}
	}
	text := htmlToText(b)
	if len(text) > 100000 {
		text = text[:100000]
	}
	// Real Claude Code answers `prompt` over the page with a small model.
	// If a model runner is wired, use it; else return the extracted text.
	if c.Answerer != nil {
		// page text is untrusted content; small-model answers only from it
		p := "Answer this question using ONLY the page content below. " +
			"If the answer isn't there, say so. Never reproduce more than " +
			"~125 chars verbatim from the page.\n\nQuestion: " + in.Prompt +
			"\n\n<page_content>\n" + text + "\n</page_content>"
		ans, err := c.Answerer(ctx, p, "")
		if err == nil {
			return Result{Content: ans}
		}
	}
	return Result{Content: "Page content:\n\n" + text}
}

type searchIn struct {
	Query          string   `json:"query"`
	AllowedDomains []string `json:"allowed_domains"`
	BlockedDomains []string `json:"blocked_domains"`
}

func execWebSearch(c *Context, raw json.RawMessage) Result {
	var in searchIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if in.Query == "" {
		return Result{Content: "query is required", IsError: true}
	}
	if c.Searcher != nil {
		res, err := c.Searcher(c.Ctx, in.Query, in.AllowedDomains, in.BlockedDomains)
		if err != nil {
			return Result{Content: "Search failed: " + err.Error(), IsError: true}
		}
		return Result{Content: res}
	}
	return Result{Content: "WebSearch is unavailable: no search backend configured", IsError: true}
}

func version() string { return "2.1.288" }

var _ = bytes.MinRead
