//go:build linux

package tools

import (
	"fmt"
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
func platformSandboxWrap(script string, s *SandboxSettings, cwd string) (string, error) {
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
		"--setenv", "TMPDIR", "/tmp",
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
		for _, p := range expandSandboxPaths(s.Credentials.Files, cwd) {
			bindIf("--ro-bind", p)
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

	if s.networkDenied() {
		args = append(args, "--unshare-net")
	}
	args = append(args, "--chdir", cwd, "/bin/sh", "-c", script)

	out := shellQuote(bwrap)
	for _, a := range args {
		out += " " + shellQuote(a)
	}
	return out, nil
}
