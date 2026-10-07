//go:build darwin

package bg

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// launchd plumbing for `claude daemon install` — real's daemon registers
// as a LaunchAgent (com.anthropic.claude-daemon); ours keeps the same
// shape under our own label. install/uninstall are explicitly opt-in.

const launchdLabel = "com.claude-go.daemon"

func launchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

// InstallDaemon writes a LaunchAgent plist and bootstraps it — the daemon
// then survives logout/reboot and auto-respawns dead bg workers.
func InstallDaemon() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array><string>%s</string><string>daemon</string><string>run</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
`, launchdLabel, exe)
	p := launchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(plist), 0o644); err != nil {
		return "", err
	}
	// already bootstrapped? kick it now; else bootstrap into the GUI domain
	if exec.Command("launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel)).Run() != nil {
		if err := exec.Command("launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), p).Run(); err != nil {
			return "", fmt.Errorf("launchctl bootstrap: %w", err)
		}
	}
	return p, nil
}

// UninstallDaemon removes the LaunchAgent — daemon state file goes too.
func UninstallDaemon() error {
	p := launchdPlistPath()
	_ = exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel)).Run()
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
