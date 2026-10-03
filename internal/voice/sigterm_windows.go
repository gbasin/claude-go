//go:build windows

package voice

import "os"

// Windows has no SIGTERM; Kill() in Stop() is the effective path.
var sigTerm = os.Kill
