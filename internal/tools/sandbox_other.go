//go:build !darwin && !linux

package tools

import (
	"fmt"
	"runtime"
)

// platformSandboxAvailable mirrors oee's default branch: sandboxing exists
// on macOS (seatbelt) and Linux (bubblewrap) only.
func platformSandboxAvailable(_ *SandboxSettings) error {
	return fmt.Errorf("sandboxing is not supported on %s", runtime.GOOS)
}

func platformSandboxWrap(script string, _ *SandboxSettings, _ string) (string, error) {
	return "", fmt.Errorf("sandboxing is not supported on %s", runtime.GOOS)
}
