//go:build windows

package repl

import (
	"fmt"
	"os"
	"os/exec"
)

// execSelf has no exec(2) on Windows: spawn the replacement attached to the
// same console and exit so it takes over the terminal.
func execSelf(argv []string) error {
	c := exec.Command(argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Start(); err != nil {
		return fmt.Errorf("spawn: %w", err)
	}
	os.Exit(0)
	return nil
}
