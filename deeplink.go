package main

// deep-link handling — real's mod_0646.js luo() parser:
//   claude://open?cwd=…&repo=owner/repo&q=…
//   claude://install-plugin?plugin=name&marketplace=owner/repo[#ref]
// --handle-uri must be the LAST arg (the OS protocol handler passes
// exactly `--handle-uri <uri>`; trailing args = injection, rejected).

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

type deepLink struct {
	action      string // "open" | "install-plugin"
	query       string // open: prefilled prompt
	cwd         string // open: resolved cwd
	repo        string // open: owner/repo
	plugin      string // install-plugin
	marketplace string
}

var (
	dlPluginRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	dlRepoRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	dlGitRefRe = regexp.MustCompile(`^[A-Za-z0-9._\-/]+$`)
	dlMktHTTPS = regexp.MustCompile(`^https://`)
	dlMktSSH   = regexp.MustCompile(`^git@[^:]+:.+`)
)

// handleURIArgCheck — real's Wwn(): "--handle-uri <uri>" must be the last
// pair on argv; extra trailing args are treated as URL-injection.
func handleURIArgCheck(argv []string) string {
	i := -1
	for j, a := range argv {
		if a == "--handle-uri" {
			i = j
		}
	}
	if i == -1 || i+1 >= len(argv) {
		return ""
	}
	if len(argv) > i+2 {
		fmt.Fprintf(os.Stderr, "claude: rejected deep-link invocation — unexpected arguments after the URI. "+
			"The OS protocol handler passes exactly `--handle-uri <uri>`; extra arguments indicate argument "+
			"injection via the URL. If invoking --handle-uri manually, place other flags before it.\n")
		os.Exit(2)
	}
	return argv[i+1]
}

func parseDeepLink(raw string) (*deepLink, error) {
	scheme := "claude"
	if !strings.HasPrefix(raw, scheme+"://") {
		if strings.HasPrefix(raw, scheme+":") {
			raw = strings.Replace(raw, scheme+":", scheme+"://", 1)
		} else {
			return nil, fmt.Errorf("Invalid deep link: expected %s:// scheme, got %q", scheme, raw)
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("Invalid deep link URL: %q", raw)
	}
	if u.Hostname() == "install-plugin" {
		if u.Fragment != "" || strings.Contains(u.RawQuery, "#") && strings.Contains(u.String(), "#") {
			return nil, fmt.Errorf(`Invalid deep link: install-plugin has a raw "#"; encode a marketplace ref as %%23 (marketplace=owner/repo%%23v1)`)
		}
		p := u.Query().Get("plugin")
		if p == "" {
			return nil, fmt.Errorf("Invalid deep link: install-plugin requires a plugin parameter")
		}
		if !dlPluginRe.MatchString(p) {
			return nil, fmt.Errorf(`Invalid plugin in deep link: expected a plugin name of ASCII letters, digits, ".", "_" and "-" (at most 100 characters)`)
		}
		mkt := u.Query().Get("marketplace")
		if err := checkMarketplace(mkt); err != nil {
			return nil, err
		}
		return &deepLink{action: "install-plugin", plugin: p, marketplace: mkt}, nil
	}
	if u.Hostname() != "open" {
		return nil, fmt.Errorf("Unknown deep link action: %q", u.Hostname())
	}
	q := u.Query()
	cwd := q.Get("cwd")
	repo := q.Get("repo")
	qs := strings.TrimSpace(q.Get("q"))
	if cwd != "" && strings.Contains(cwd, "..") {
		return nil, fmt.Errorf("Invalid deep link: cwd may not contain '..'")
	}
	if repo != "" && !dlRepoRe.MatchString(repo) {
		return nil, fmt.Errorf("Invalid repo in deep link: expected %q, got %q", "owner/repo", repo)
	}
	return &deepLink{action: "open", query: qs, cwd: cwd, repo: repo}, nil
}

// checkMarketplace — real's krt(): owner/repo, https://, or git@ ssh URL;
// optional #ref or @ref; no ".." or empty segments; no % escapes.
func checkMarketplace(m string) error {
	if m == "" {
		return nil
	}
	if len(m) > 256 {
		return fmt.Errorf("Deep link marketplace exceeds the length limit")
	}
	src := m
	ref := ""
	if i := strings.Index(m, "#"); i >= 0 {
		src, ref = m[:i], m[i+1:]
	} else if i := strings.Index(m, "@"); i > 0 && !strings.Contains(m[:i], ":") {
		src, ref = m[:i], m[i+1:]
	}
	if strings.Contains(src, "%") {
		return fmt.Errorf("Invalid marketplace in deep link: percent-encoded characters are not allowed in the source")
	}
	if !dlRepoRe.MatchString(src) && !dlMktHTTPS.MatchString(src) && !dlMktSSH.MatchString(src) {
		return fmt.Errorf(`Invalid marketplace in deep link: expected a GitHub "owner/repo", an https:// URL, or a git SSH URL like git@host:owner/repo.git`)
	}
	if strings.Contains(src, "//") && !dlMktHTTPS.MatchString(src) && !dlMktSSH.MatchString(src) {
		return fmt.Errorf(`Invalid marketplace in deep link: ".", ".." and empty path segments are not allowed`)
	}
	if ref != "" && !dlGitRefRe.MatchString(ref) {
		return fmt.Errorf("Invalid marketplace in deep link: the git ref after \"#\" or \"@\" may only contain letters, digits, \".\", \"_\", \"-\" and \"/\"")
	}
	return nil
}
