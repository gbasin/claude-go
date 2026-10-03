//go:build darwin

package tools

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Seatbelt path lists from mod_0386.js (_jn / kjn / Sct): the mandatory
// deny-read set, the read-allow exceptions inside it, and the device nodes
// that stay accessible.
var seatbeltDenyRead = []string{
	"/etc", "/var/root", "/var/db", "/var/log",
	"/opt/homebrew/etc", "/opt/homebrew/var",
	"/opt/local/etc", "/opt/local/var",
	"/usr/local/etc", "/usr/local/var",
	"/Library/Preferences", "/Library/Application Support",
	"/Library/Keychains", "/Library/LaunchDaemons", "/Library/LaunchAgents",
	"/Users", "/Volumes",
}

var seatbeltAllowRead = []string{
	"/etc/passwd", "/etc/group", "/etc/hosts", "/etc/localtime",
	"/etc/paths", "/etc/paths.d",
	"/etc/ssl/cert.pem", "/etc/ssl/certs", "/etc/ssl/openssl.cnf",
	"/var/db/timezone",
}

var seatbeltDevices = []string{
	"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom",
	"/dev/stdin", "/dev/stdout", "/dev/stderr",
	"/dev/fd", "/dev/dtracehelper", "/dev/autofs_nowait",
}

var seatbeltPath = sync.OnceValues(func() (string, error) {
	return exec.LookPath("sandbox-exec")
})

// platformSandboxAvailable implements oee's macos branch: seatbelt only.
func platformSandboxAvailable(_ *SandboxSettings) error {
	_, err := seatbeltPath()
	if err != nil {
		return fmt.Errorf("sandbox-exec not found")
	}
	return nil
}

// sbQuote emits an SBPL double-quoted string literal.
func sbQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// sbPathFilter emits an SBPL filter for one path setting: a regex filter for
// globs, otherwise literal+subpath (covers files, dirs, and not-yet-existing
// paths).
func sbPathFilter(b *strings.Builder, p string) {
	if strings.ContainsAny(p, "*?") {
		if re := globToRegexp(p); re != nil {
			fmt.Fprintf(b, "\t(regex #%s)\n", sbQuote(re.String()))
			return
		}
	}
	fmt.Fprintf(b, "\t(literal %s)\n\t(subpath %s)\n", sbQuote(p), sbQuote(p))
}

func sbRule(b *strings.Builder, op string, paths []string) {
	if len(paths) == 0 {
		return
	}
	fmt.Fprintf(b, "(%s\n", op)
	for _, p := range paths {
		sbPathFilter(b, p)
	}
	b.WriteString(")\n")
}

// buildSeatbeltProfile renders the .sb profile. Rule order matters: SBPL is
// last-match-wins, so denies are emitted before the allows that carve
// exceptions out of them.
func buildSeatbeltProfile(s *SandboxSettings, cwd string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")

	if !s.Filesystem.Disabled {
		home, _ := os.UserHomeDir()
		real := func(p string) string {
			if r, err := filepath.EvalSymlinks(p); err == nil {
				return r
			}
			return p
		}
		// Seatbelt evaluates resolved paths (/etc -> /private/etc), so every
		// rule gets the realpath variant of each entry in addition.
		withReal := func(paths []string) []string {
			out := append([]string{}, paths...)
			for _, p := range paths {
				if r := real(p); r != p {
					out = append(out, r)
				}
			}
			return out
		}
		cwdReal := real(cwd)

		// writable set: cwd + temp dirs + user allowWrite
		writable := []string{cwd}
		if cwdReal != cwd {
			writable = append(writable, cwdReal)
		}
		for _, t := range []string{"/tmp", "/private/tmp", os.TempDir(), "/var/folders", "/private/var/folders"} {
			if t != "" {
				writable = append(writable, t)
			}
		}
		writable = append(writable, expandSandboxPaths(s.Filesystem.AllowWrite, cwd)...)

		// deny-read: mandatory list + /dev + user denyRead + credential files
		denyRead := append([]string{}, seatbeltDenyRead...)
		denyRead = append(denyRead, "/dev")
		denyRead = append(denyRead, expandSandboxPaths(s.Filesystem.DenyRead, cwd)...)
		denyRead = append(denyRead, expandSandboxPaths(s.Credentials.Files, cwd)...)
		if home != "" {
			denyRead = append(denyRead, home)
		}

		// allow-read: exceptions + devices + everything writable + allowRead.
		// Writable paths must stay readable: /Users and $HOME are denied for
		// read, so cwd and allowWrite need explicit read grants.
		allowRead := append([]string{}, seatbeltAllowRead...)
		allowRead = append(allowRead, writable...)
		allowRead = append(allowRead, expandSandboxPaths(s.allowRead(), cwd)...)

		// Traversal grant: a file-read* deny on a dir also denies
		// file-read-metadata on it, which blocks reaching any allowed leaf
		// inside. Re-allow metadata on the denied dirs themselves and on
		// every ancestor of an allowed path (metadata only exposes that the
		// name exists; listing and reading stay denied).
		meta := map[string]bool{}
		ancestors := func(p string) {
			for d := filepath.Dir(p); d != "/" && d != "." && d != p; d = filepath.Dir(d) {
				meta[d] = true
			}
		}
		for _, p := range denyRead {
			meta[p] = true
			ancestors(p)
		}
		for _, p := range allowRead {
			ancestors(p)
		}
		for _, p := range seatbeltDevices {
			ancestors(p)
		}
		for _, p := range writable {
			ancestors(p)
		}
		var metaList []string
		for p := range meta {
			if strings.ContainsAny(p, "*?[") {
				continue // glob-derived dirs can't be metadata literals
			}
			metaList = append(metaList, p)
		}

		sbRule(&b, "deny file-read*", withReal(denyRead))
		sbRule(&b, "allow file-read-metadata", withReal(metaList))
		sbRule(&b, "allow file-read*", withReal(allowRead))

		// writes: deny all, then allow the writable set, then re-deny
		// denyWrite (which takes precedence over allowWrite).
		sbRule(&b, "deny file-write*", []string{"/"})
		sbRule(&b, "allow file-write*", withReal(writable))
		denyWrite := expandSandboxPaths(s.Filesystem.DenyWrite, cwd)
		denyWrite = append(denyWrite, expandSandboxPaths(s.Credentials.Files, cwd)...)
		sbRule(&b, "deny file-write*", withReal(denyWrite))

		// devices: read+write (literal for nodes, subpath for /dev/fd).
		// Emitted last so /dev/null etc. stay writable despite the
		// deny-write-all and the /dev deny-read above.
		b.WriteString("(allow file-read* file-write*\n")
		for _, d := range seatbeltDevices {
			if d == "/dev/fd" {
				fmt.Fprintf(&b, "\t(subpath %s)\n", sbQuote(d))
			} else {
				fmt.Fprintf(&b, "\t(literal %s)\n", sbQuote(d))
			}
		}
		b.WriteString(")\n")
	}

	// network: seatbelt can't filter per-domain (that needs the SRT proxy);
	// only full deny is expressible.
	if s.networkDenied() {
		b.WriteString("(deny network*)\n")
		// allowUnixSockets: SBPL has no per-path AF_UNIX grant distinct from
		// network*, so explicit unix-socket allows can't be expressed once
		// network* is denied; allowAllUnixSockets likewise implies no deny.
		if s.Network != nil && s.Network.AllowAllUnixSockets {
			b.WriteString("(allow network* (subpath \"/var\"))\n")
		}
	}
	return b.String()
}

// platformSandboxWrap writes the seatbelt profile to a stable path under the
// OS temp dir and returns the sandbox-exec command line. The whole script
// (command + cwd sentinel) runs inside the sandboxed shell so `cd` tracking
// still works.
func platformSandboxWrap(script string, s *SandboxSettings, cwd string) (string, error) {
	se, err := seatbeltPath()
	if err != nil {
		return "", fmt.Errorf("sandbox-exec not found")
	}
	profile := buildSeatbeltProfile(s, cwd)
	dir := filepath.Join(os.TempDir(), "claude-go-sandbox")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(profile))
	pf := filepath.Join(dir, fmt.Sprintf("profile-%x.sb", sum[:8]))
	if _, err := os.Stat(pf); err != nil {
		if err := os.WriteFile(pf, []byte(profile), 0o644); err != nil {
			return "", err
		}
	}
	sh := os.Getenv("SHELL")
	if sh == "" || !strings.HasSuffix(sh, "sh") {
		sh = "/bin/bash"
	}
	return fmt.Sprintf("%s -f %s %s -c %s", shellQuote(se), shellQuote(pf), shellQuote(sh), shellQuote(script)), nil
}
