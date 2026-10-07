//go:build windows

package repl

// No job-control signals on Windows; isSuspendKey is gated on this.
const suspendSupported = false

func suspendToShell(ks *keySource, repaint func()) {}
