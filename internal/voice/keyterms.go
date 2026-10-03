package voice

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// baseKeyterms is Q in mod_1711 — the always-on STT keyterm seed list.
var baseKeyterms = []string{
	"MCP", "symlink", "grep", "regex", "localhost", "codebase",
	"TypeScript", "JSON", "OAuth", "webhook", "gRPC", "dotfiles",
	"subagent", "worktree",
}

const keytermLimit = 50       // B in mod_1711
const keytermHeaderCap = 1024 // H in mod_1710 — joined header char budget

var (
	camelSplit = regexp.MustCompile(`([a-z])([A-Z])`)
	termSep    = regexp.MustCompile(`[-_./\s]+`)
)

// splitTerms is K(): split camelCase boundaries then split on
// [-_./\s]+; keep terms with 2 < len <= 20.
func splitTerms(name string) []string {
	name = camelSplit.ReplaceAllString(name, "$1 $2")
	var out []string
	for _, t := range termSep.Split(name, -1) {
		t = strings.TrimSpace(t)
		if len(t) > 2 && len(t) <= 20 {
			out = append(out, t)
		}
	}
	return out
}

// Keyterms is O() in mod_1711: base list + cwd basename + splitTerms of
// each extra file name (basename, extension stripped), deduped, capped at
// 50 entries in insertion order.
func Keyterms(cwd string, extraFiles []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, k := range baseKeyterms {
		add(k)
	}
	if cwd != "" {
		if base := filepath.Base(cwd); len(base) > 2 && len(base) <= 50 {
			add(base)
		}
	}
	for _, f := range extraFiles {
		if len(out) >= keytermLimit {
			break
		}
		base := filepath.Base(f)
		if i := strings.LastIndex(base, "."); i > 0 {
			base = base[:i]
		}
		for _, t := range splitTerms(base) {
			add(t)
		}
	}
	if len(out) > keytermLimit {
		out = out[:keytermLimit]
	}
	return out
}

var nonASCII = regexp.MustCompile(`[^\x20-\x7E]`)

// SanitizeKeyterms is j() in mod_1710: strip commas + non-ASCII from each
// term, squash whitespace, dedupe, and join under a 1024-char budget.
func SanitizeKeyterms(terms []string) string {
	seen := map[string]bool{}
	var parts []string
	total := 0
	for _, t := range terms {
		p := strings.ReplaceAll(t, ",", " ")
		p = nonASCII.ReplaceAllString(p, "")
		p = strings.Join(strings.Fields(p), " ")
		if p == "" || seen[p] {
			continue
		}
		extra := len(p)
		if len(parts) > 0 {
			extra++
		}
		if total+extra > keytermHeaderCap {
			break
		}
		seen[p] = true
		parts = append(parts, p)
		total += extra
	}
	return strings.Join(parts, ",")
}

func getenvBool(name string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch v {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}
