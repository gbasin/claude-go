//go:build linux

package bg

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// systemd user-unit plumbing for `claude daemon install` — same role as
// real's installable service, scoped to the user's systemd.

const unitName = "claude-go-daemon.service"

func unitPath() (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "systemd", "user", unitName), nil
}

// InstallDaemon writes a user systemd unit and enables it — the daemon
// starts at login and respawns dead bg workers.
func InstallDaemon() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	unit := fmt.Sprintf(`[Unit]
Description=claude-go background session supervisor

[Service]
ExecStart=%s daemon run
Restart=on-failure

[Install]
WantedBy=default.target
`, exe)
	p, err := unitPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(unit), 0o644); err != nil {
		return "", err
	}
	if err := exec.Command("systemctl", "--user", "daemon-reload").Run(); err != nil {
		return "", fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if err := exec.Command("systemctl", "--user", "enable", "--now", unitName).Run(); err != nil {
		return "", fmt.Errorf("systemctl enable: %w", err)
	}
	return p, nil
}

// UninstallDaemon disables and removes the user unit.
func UninstallDaemon() error {
	p, _ := unitPath()
	_ = exec.Command("systemctl", "--user", "disable", "--now", unitName).Run()
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return nil
}
