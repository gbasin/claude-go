// sandbox.credentials masking pipeline — the Go port of real's v_ (env
// vars) and x_ (files) from mod_0222.js, plus its mode:"deny" plumbing.
//
//   - mode "deny" env vars are unset in the child; files go on the read-deny
//     list (on Linux, a /dev/null or tmpfs mask).
//   - mode "mask" env vars/files are replaced inside the sandbox with
//     "fake_value_..." sentinels registered on the egress proxy; the proxy
//     swaps sentinel→real on requests to hosts matching injectHosts
//     (default: the effective network.allowedDomains host patterns).
//   - On macOS real degrades masked files to read-deny ("file mask degrades
//     to deny on macOS until the interposer lands"); we do the same.
//   - extract/maskClaims/decode:"jwt" behave per real's schema; a no-match
//     is governed by onExtractNoMatch: warn (pass through unmasked), deny
//     (unset/degrade to deny), error (fail the wrap).
package tools

import (
	"fmt"
	"github.com/gbasin/claude-go/internal/netproxy"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"unicode/utf8"
)

// credPlan is prepareCredentials' output: env deltas, extra read-deny
// paths, masked-file binds (Linux bwrap only), and warnings to surface.
type credPlan struct {
	setEnv   []string    // "NAME=<masked>" overrides
	unsetEnv []string    // deny-mode + degradeToUnsetNames
	denyRead []string    // host paths the sandbox must not read
	binds    [][2]string // {realPath, fakePath} for linux --ro-bind
	warns    []string
}

// maskedFileStore is real's W0 — a per-process temp dir of "*.fake" files
// holding masked content bound over credential paths (Linux) or simply
// standing in for them (sentinels register either way).
var maskedFileStore struct {
	once sync.Once
	mu   sync.Mutex
	dir  string
	n    int
}

func writeMaskedFile(content string) (string, error) {
	maskedFileStore.once.Do(func() {
		maskedFileStore.dir, _ = os.MkdirTemp("", "claude-go-credmask-")
	})
	maskedFileStore.mu.Lock()
	defer maskedFileStore.mu.Unlock()
	if maskedFileStore.dir == "" {
		return "", fmt.Errorf("cannot create credential-mask store dir")
	}
	maskedFileStore.n++
	p := filepath.Join(maskedFileStore.dir, strconv.Itoa(maskedFileStore.n)+".fake")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		return "", err
	}
	return p, nil
}

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// defaultInjectHosts is real's `(allowedDomains??[]).map(RBt)` — the host
// pattern of each effective allowedDomains entry (":port" stripped; CIDR
// entries stay — they simply never match a hostname).
func defaultInjectHosts(n *SandboxNetwork) []string {
	if n == nil {
		return nil
	}
	out := make([]string, 0, len(n.effectiveAllowed()))
	for _, d := range n.effectiveAllowed() {
		h, _ := parseRuleHost(d)
		out = append(out, h)
	}
	return out
}

// parseRuleHost splits ":port" off a domain entry — a local copy of
// netproxy's parseRule (unexported) covering the host-pattern cases.
func parseRuleHost(s string) (string, int) {
	if len(s) > 0 && s[0] == '[' {
		if i := indexByteStr(s, ']'); i >= 0 {
			rest := s[i+1:]
			if rest == "" {
				return s[1:i], 0
			}
			if rest[0] == ':' {
				if p, err := strconv.Atoi(rest[1:]); err == nil {
					return s[1:i], p
				}
			}
			return s, 0
		}
	}
	last, nc := -1, 0
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			nc++
			last = i
		}
	}
	if nc == 1 && last > 0 {
		if p, err := strconv.Atoi(s[last+1:]); err == nil {
			return s[:last], p
		}
	}
	return s, 0
}

func indexByteStr(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// noMatchDesc builds the "what matched nothing" clause real embeds in its
// warn/deny/error messages (extract vs decode:"jwt" wordings).
func noMatchDesc(extract, decode string, maskClaims []string, pattern string) string {
	if decode == "jwt" {
		if len(maskClaims) > 0 {
			return fmt.Sprintf(`decode "jwt" with pattern %q that matched no verified JWT with maskable claims (maskClaims: %s)`,
				pattern, fmt.Sprint(maskClaims))
		}
		return fmt.Sprintf(`decode "jwt" with pattern %q that matched no verified JWT`, pattern)
	}
	return fmt.Sprintf(`extract pattern %q that matched nothing`, extract)
}

// prepareCredentials implements real's Cy: fold credentials config into
// env deltas + deny-read paths + masked-file binds, registering every
// sentinel on the egress proxy's CredStore. Without a proxy the sandbox
// still masks/denies — the sentinel just never swaps back on egress (same
// safe degradation as real running without egress hooks).
func (s *SandboxSettings) prepareCredentials(px *netproxy.Proxy, cwd string) (*credPlan, error) {
	var store *netproxy.CredStore
	if px != nil {
		store = px.Creds
		px.AllowPlaintextInject = s.Credentials.AllowPlaintextInject
	} else {
		store = netproxy.NewCredStore()
	}
	defaults := defaultInjectHosts(s.Network)
	hosts := func(explicit []string) []string {
		if explicit != nil {
			return explicit
		}
		return defaults
	}
	out := &credPlan{}
	maskedAnyFile := false
	// whole-value masked env vars — the only entries usable as SigV4
	// pair members (real: extract/decode entries don't qualify, re-signing
	// needs the whole real value).
	maskedWhole := map[string]maskedWholeVar{}

	for _, e := range s.Credentials.EnvVars {
		if !envNameRe.MatchString(e.Name) {
			out.warns = append(out.warns, fmt.Sprintf(
				`[credential-mask] credentials.envVars entry %q is not a valid env var name — skipped`, e.Name))
			continue
		}
		if e.Mode == "deny" {
			out.unsetEnv = append(out.unsetEnv, e.Name)
			continue
		}
		if e.Mode != "mask" {
			out.warns = append(out.warns, fmt.Sprintf(
				`[credential-mask] credentials.envVars entry %q has unknown mode %q — skipped`, e.Name, e.Mode))
			continue
		}
		val, present := os.LookupEnv(e.Name)
		if !present {
			continue
		}
		inj := hosts(e.InjectHosts)
		name := "env:" + e.Name

		if e.Decode == "jwt" {
			// real's env path verifies the whole value as a JWT (no extract
			// scan — that's the files path only).
			if !netproxy.IsJWT(val) {
				out.warns = append(out.warns, fmt.Sprintf(
					`[sandbox-runtime] WARNING: credentials.envVars entry %q has decode "jwt" but its value did not verify as a JWT. The variable is left UNPROTECTED (real value visible as-is inside the sandbox). Fix the config or remove the entry.`, e.Name))
				continue
			}
			if len(e.MaskClaims) > 0 {
				fake, _, ok := netproxy.MaskJWTClaims(val, e.MaskClaims,
					func(claim, cval string) string {
						return store.Register(name+"#"+claim, cval, inj)
					})
				if !ok {
					out.warns = append(out.warns, fmt.Sprintf(
						`[sandbox-runtime] WARNING: credentials.envVars entry %q has maskClaims %v but none is present as a string claim in its JWT value. The variable is left UNPROTECTED (real value visible as-is inside the sandbox). Fix the config or remove the entry.`, e.Name, e.MaskClaims))
					continue
				}
				store.RegisterWithSentinel(name, fake, val, inj)
				out.setEnv = append(out.setEnv, e.Name+"="+fake)
				continue
			}
			fake := netproxy.FakeJWT()
			store.RegisterWithSentinel(name, fake, val, inj)
			out.setEnv = append(out.setEnv, e.Name+"="+fake)
			continue
		}
		if e.Decode != "" {
			out.warns = append(out.warns, fmt.Sprintf(
				`[credential-mask] credentials.envVars entry %q: unsupported decode %q — entry skipped`, e.Name, e.Decode))
			continue
		}
		if e.Extract == "" {
			// whole-value mask
			sent := store.Register(e.Name, val, inj)
			maskedWhole[e.Name] = maskedWholeVar{sentinel: sent, injectHosts: inj}
			out.setEnv = append(out.setEnv, e.Name+"="+sent)
			continue
		}
		re, err := compileExtract(e.Extract)
		if err != nil {
			out.warns = append(out.warns, fmt.Sprintf(
				`[credential-mask] credentials.envVars entry %q: %v — entry skipped`, e.Name, err))
			continue
		}
		masked, ok, err := netproxy.MaskedValue(netproxy.MaskSpec{
			Value: val, Extract: re, MaskDuplicates: e.MaskDuplicates,
			Register:         func(n2, real string) string { return store.Register(n2, real, inj) },
			RegisterExplicit: func(n2, sent, real string) { store.RegisterWithSentinel(n2, sent, real, inj) },
			NameFn:           func(idx int) string { return fmt.Sprintf("%s#%d", name, idx) },
			ClaimFn:          func(occ int, claim string) string { return fmt.Sprintf("%s#%s", name, claim) },
		})
		if err != nil {
			out.warns = append(out.warns, fmt.Sprintf(
				`[credential-mask] credentials.envVars entry %q: %v — entry skipped`, e.Name, err))
			continue
		}
		if !ok {
			switch e.OnExtractNoMatch {
			case "error":
				return nil, fmt.Errorf(`credentials.envVars entry %q: %s (onExtractNoMatch: "error"). Fix the regex, change to "warn"/"deny", or remove the entry.`,
					e.Name, noMatchDesc(e.Extract, e.Decode, e.MaskClaims, e.Extract))
			case "deny":
				out.unsetEnv = append(out.unsetEnv, e.Name)
			default:
				out.warns = append(out.warns, fmt.Sprintf(
					`[sandbox-runtime] WARNING: credentials.envVars entry %q has %s in its value. The variable is left UNPROTECTED (real value visible as-is inside the sandbox). Fix the config, set onExtractNoMatch to "deny" or "error", or remove the entry.`,
					e.Name, noMatchDesc(e.Extract, e.Decode, e.MaskClaims, e.Extract)))
			}
			continue
		}
		out.setEnv = append(out.setEnv, e.Name+"="+masked)
	}

	for _, e := range s.Credentials.Files {
		path := expandSandboxPath(e.Path, cwd)
		if e.Mode == "deny" {
			out.denyRead = append(out.denyRead, path)
			continue
		}
		if e.Mode != "mask" {
			out.warns = append(out.warns, fmt.Sprintf(
				`[credential-mask] credentials.files entry %q has unknown mode %q — skipped`, e.Path, e.Mode))
			continue
		}
		fi, err := os.Stat(path)
		if err != nil {
			out.warns = append(out.warns, fmt.Sprintf(
				"[credential-mask] Skipping masked file (unreadable on host): %s — %v", e.Path, err))
			continue
		}
		if fi.IsDir() {
			out.warns = append(out.warns, fmt.Sprintf(
				`[credential-mask] Skipping masked file entry that resolves to a directory: %s — use mode "deny" for directories.`, e.Path))
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			out.warns = append(out.warns, fmt.Sprintf(
				"[credential-mask] Skipping masked file (unreadable on host): %s — %v", e.Path, err))
			continue
		}
		if !utf8.Valid(raw) {
			out.warns = append(out.warns, fmt.Sprintf(
				"[credential-mask] Skipping masked file with non-UTF-8 content (binary credential files are not supported in mask mode): %s", e.Path))
			continue
		}
		if e.Decode != "" && e.Decode != "jwt" {
			out.warns = append(out.warns, fmt.Sprintf(
				`[credential-mask] credentials.files entry %q: unsupported decode %q — entry skipped`, e.Path, e.Decode))
			continue
		}
		var re *regexp.Regexp
		if e.Extract != "" {
			if re, err = compileExtract(e.Extract); err != nil {
				out.warns = append(out.warns, fmt.Sprintf(
					`[credential-mask] credentials.files entry %q: %v — entry skipped`, e.Path, err))
				continue
			}
		}
		content := string(raw)
		key := "file:" + path
		inj := hosts(e.InjectHosts)
		masked, ok, err := netproxy.MaskedValue(netproxy.MaskSpec{
			Value: content, Extract: re, DecodeJWT: e.Decode == "jwt",
			MaskClaims: e.MaskClaims, MaskDuplicates: e.MaskDuplicates,
			WholeName:        key,
			Register:         func(n2, real string) string { return store.Register(n2, real, inj) },
			RegisterExplicit: func(n2, sent, real string) { store.RegisterWithSentinel(n2, sent, real, inj) },
			NameFn:           func(idx int) string { return fmt.Sprintf("%s#%d", key, idx) },
			ClaimFn:          func(occ int, claim string) string { return fmt.Sprintf("%s#jwt%d.%s", key, occ, claim) },
		})
		if err != nil {
			out.warns = append(out.warns, fmt.Sprintf(
				`[credential-mask] credentials.files entry %q: %v — entry skipped`, e.Path, err))
			continue
		}
		if !ok {
			pat := e.Extract
			if pat == "" {
				pat = `(eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)`
			}
			switch e.OnExtractNoMatch {
			case "error":
				return nil, fmt.Errorf(`credentials.files entry %q: %s (onExtractNoMatch: "error"). Fix the config or remove the entry.`,
					e.Path, noMatchDesc(e.Extract, e.Decode, e.MaskClaims, pat))
			case "deny":
				out.denyRead = append(out.denyRead, path)
			default:
				out.warns = append(out.warns, fmt.Sprintf(
					`[sandbox-runtime] WARNING: credentials.files entry %q has %s in the file. The file is left UNPROTECTED (readable as-is inside the sandbox). Fix the config, set onExtractNoMatch to "deny" or "error", or remove the entry.`,
					e.Path, noMatchDesc(e.Extract, e.Decode, e.MaskClaims, pat)))
			}
			continue
		}
		fake, err := writeMaskedFile(masked)
		if err != nil {
			out.warns = append(out.warns, fmt.Sprintf("[credential-mask] cannot stage masked file for %s: %v", e.Path, err))
			continue
		}
		if runtime.GOOS == "darwin" {
			// seatbelt can't bind-mount a fake file over the real path:
			// masked files degrade to read-deny (real does the same).
			maskedAnyFile = true
			out.denyRead = append(out.denyRead, path)
		} else {
			out.binds = append(out.binds, [2]string{path, fake})
		}
	}
	if maskedAnyFile {
		out.warns = append(out.warns,
			"[Sandbox macOS] file mask degrades to deny on macOS until the interposer lands")
	}
	s.registerAWSPairs(px, maskedWhole, defaults, out)
	return out, nil
}

// maskedWholeVar records a whole-value masked env var for SigV4 pairing:
// its registered sentinel (what the sandboxed SDK puts in x-amz-credential)
// plus the resolved inject scope.
type maskedWholeVar struct {
	sentinel    string
	injectHosts []string
}

// Conventional AWS env names — real's __/T_/w_. When no configured pair
// mentions any of them, the conventional trio is auto-paired.
const (
	awsAccessKeyIDVar     = "AWS_ACCESS_KEY_ID"
	awsSecretAccessKeyVar = "AWS_SECRET_ACCESS_KEY"
	awsSessionTokenVar    = "AWS_SESSION_TOKEN"
)

// registerAWSPairs is real's C_: fold awsPairs + the conventional
// auto-pair into the proxy's SigV4 re-sign registry. A pair registers
// only when BOTH the key-id and secret vars are whole-value masked and
// present in the environment; a pair whose only unusable member is the
// session token still re-signs, without x-amz-security-token.
func (s *SandboxSettings) registerAWSPairs(px *netproxy.Proxy, masked map[string]maskedWholeVar, defaults []string, out *credPlan) {
	if px == nil || px.AWSPairs == nil {
		return
	}
	if s.Credentials.SigV4 != nil {
		px.SigV4 = netproxy.SigV4Policy{
			Streaming: s.Credentials.SigV4.Streaming,
			Presigned: s.Credentials.SigV4.Presigned,
			SigV4a:    s.Credentials.SigV4.SigV4a,
		}
	}
	pairs := append([]SandboxAWSPair(nil), s.Credentials.AWSPairs...)
	claimed := map[string]bool{}
	for _, pr := range pairs {
		claimed[pr.AccessKeyIDVar] = true
		claimed[pr.SecretAccessKeyVar] = true
		if pr.SessionTokenVar != "" {
			claimed[pr.SessionTokenVar] = true
		}
	}
	if !claimed[awsAccessKeyIDVar] && !claimed[awsSecretAccessKeyVar] && !claimed[awsSessionTokenVar] {
		pairs = append(pairs, SandboxAWSPair{
			AccessKeyIDVar:     awsAccessKeyIDVar,
			SecretAccessKeyVar: awsSecretAccessKeyVar,
			SessionTokenVar:    awsSessionTokenVar,
		})
	}
	for _, pr := range pairs {
		km := masked[pr.AccessKeyIDVar]
		secretMasked := masked[pr.SecretAccessKeyVar].sentinel != ""
		realKey, keyOK := os.LookupEnv(pr.AccessKeyIDVar)
		realSecret, secretOK := os.LookupEnv(pr.SecretAccessKeyVar)
		if km.sentinel == "" || !secretMasked || !keyOK || !secretOK {
			if secretMasked && km.sentinel == "" {
				warn := fmt.Sprintf(
					`[sandbox-runtime] WARNING: "%s" is masked but "%s" is not, so no AWS credential pair was registered. SigV4 requests will be signed with the masked placeholder secret and the proxy cannot detect or re-sign them — they will fail upstream. Mask both variables (whole-value) to enable re-signing.`,
					pr.SecretAccessKeyVar, pr.AccessKeyIDVar)
				out.warns = append(out.warns, warn)
				if px.Logf != nil {
					px.Logf("%s", warn)
				}
			}
			continue
		}
		inj := km.injectHosts
		if inj == nil {
			inj = defaults
		}
		realToken := ""
		if pr.SessionTokenVar != "" && masked[pr.SessionTokenVar].sentinel != "" {
			realToken = os.Getenv(pr.SessionTokenVar)
		}
		px.AWSPairs.Register(km.sentinel, realKey, realSecret, realToken, inj)
		extra := ""
		if realToken != "" {
			extra = " (+session token)"
		}
		if px.Logf != nil {
			px.Logf("[credential-aws] registered SigV4 pair for %s/%s%s",
				pr.AccessKeyIDVar, pr.SecretAccessKeyVar, extra)
		}
	}
}

// compileExtract validates a credentials extract regex — real's schema
// requires group 1 to exist.
func compileExtract(pattern string) (*regexp.Regexp, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("extract is not a valid regular expression: %v", err)
	}
	if re.NumSubexp() < 1 {
		return nil, fmt.Errorf(`extract must contain at least one capturing group — group 1 is the credential value to mask (e.g. "token:\s*(\S+)")`)
	}
	return re, nil
}
