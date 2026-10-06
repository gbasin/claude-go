//go:build !darwin && !linux

package bg

import "fmt"

// InstallDaemon / UninstallDaemon are unsupported off darwin/linux —
// no service manager wiring exists for this platform.
func InstallDaemon() (string, error) {
	return "", fmt.Errorf("daemon install isn't supported on this platform — use `claude daemon start`")
}

func UninstallDaemon() error {
	return fmt.Errorf("daemon install isn't supported on this platform")
}
