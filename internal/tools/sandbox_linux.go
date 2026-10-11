//go:build linux

package tools

import (
	"fmt"
	"github.com/gbasin/claude-go/internal/netproxy"
	"os"
	"os/exec"
	"sync"
)

// bwrapBin resolves the bwrap binary: sandbox.bwrapPath from settings wins,
// else PATH lookup (cached).
var bwrapPathCache sync.Map // settings-path key -> resolved binary / error

func bwrapBin(s *SandboxSettings) (string, error) {
	key := ""
	if s != nil {
		key = s.BwrapPath
	}
	if v, ok := bwrapPathCache.Load(key); ok {
		return v.(bwrapResult).path, v.(bwrapResult).err
	}
	var r bwrapResult
	if key != "" {
		r = bwrapResult{path: key}
	} else if p, err := exec.LookPath("bwrap"); err != nil {
		r = bwrapResult{err: fmt.Errorf("bwrap not found")}
	} else {
		r = bwrapResult{path: p}
	}
	bwrapPathCache.Store(key, r)
	return r.path, r.err
}

type bwrapResult struct {
	path string
	err  error
}

// bwrapProbe verifies bwrap actually launches (user namespaces enabled,
// setuid helper present). Result cached per binary.
var bwrapProbeCache sync.Map // binary path -> bwrapResult.err

func bwrapProbe(s *SandboxSettings) error {
	p, err := bwrapBin(s)
	if err != nil {
		return err
	}
	if v, ok := bwrapProbeCache.Load(p); ok {
		return v.(bwrapResult).err
	}
	cmd := exec.Command(p, "--ro-bind", "/", "/", "--die-with-parent", "/bin/true")
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		runErr = fmt.Errorf("bwrap probe failed: %v: %s", runErr, out)
	}
	bwrapProbeCache.Store(p, bwrapResult{err: runErr})
	return runErr
}

// platformSandboxAvailable implements oee's linux branch: bubblewrap.
func platformSandboxAvailable(s *SandboxSettings) error {
	return bwrapProbe(s)
}

// platformSandboxWrap builds `bwrap ... /bin/sh -c script`.
// Mount order matters: later binds shadow earlier ones, so the writable set
// goes on, then denyWrite read-only re-binds, then denyRead masks, then
// allowRead unmasks (ro-bind sources resolve against the host fs).
// cred carries the credential-mask plan: deny paths get masked, mask files
// get a --ro-bind of their sentinel content (real's maskedFileBinds).
func platformSandboxWrap(script string, s *SandboxSettings, cwd string, px *netproxy.Proxy, cred *credPlan) (string, error) {
	bwrap, err := bwrapBin(s)
	if err != nil {
		return "", fmt.Errorf("bwrap not found: %w", err)
	}
	args := []string{
		"--die-with-parent", "--new-session",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
		"--dir", "/tmp/claude", // real's TMPDIR target exists inside the tmpfs
		"--setenv", "TMPDIR", "/tmp/claude",
	}

	bindIf := func(flag string, p string) {
		if _, err := os.Stat(p); err == nil {
			args = append(args, flag, p, p)
		}
	}
	maskRead := func(p string) {
		fi, err := os.Stat(p)
		if err != nil {
			return
		}
		if fi.IsDir() {
			args = append(args, "--tmpfs", p)
		} else {
			args = append(args, "--ro-bind", "/dev/null", p)
		}
	}

	if !s.Filesystem.Disabled {
		// writable: cwd + allowWrite (+ /tmp via --tmpfs above)
		bindIf("--bind", cwd)
		for _, p := range expandSandboxPaths(s.Filesystem.AllowWrite, cwd) {
			bindIf("--bind", p)
		}
		// denyWrite beats allowWrite
		for _, p := range expandSandboxPaths(s.Filesystem.DenyWrite, cwd) {
			bindIf("--ro-bind", p)
		}
		if cred != nil {
			// credential mode:"deny" + degraded paths are unreadable…
			for _, p := range cred.denyRead {
				maskRead(p)
			}
			// …while mode:"mask" files bind their sentinel content in place
			for _, b := range cred.binds {
				args = append(args, "--ro-bind", b[1], b[0])
			}
		}
		// denyRead masked, then allowRead re-exposed
		for _, p := range expandSandboxPaths(s.Filesystem.DenyRead, cwd) {
			maskRead(p)
		}
		for _, p := range expandSandboxPaths(s.allowRead(), cwd) {
			bindIf("--ro-bind", p)
		}
	} else {
		bindIf("--bind", cwd)
	}

	// With the proxy live we keep the shared netns so the sandboxed command
	// can reach it on loopback (real enforces egress via seccomp-bpf — not
	// implemented, so direct egress is possible here). Without it, deny-all
	// (or a failed proxy) collapses to an isolated netns.
	if px == nil && (s.networkDenied() || s.needsNetworkProxy()) {
		args = append(args, "--unshare-net")
	}
	args = append(args, "--chdir", cwd, "/bin/sh", "-c", script)

	out := shellQuote(bwrap)
	for _, a := range args {
		out += " " + shellQuote(a)
	}
	return out, nil
}
