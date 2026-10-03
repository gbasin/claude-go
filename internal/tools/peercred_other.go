//go:build !darwin && !linux

package tools

import (
	"net"
	"os"
)

// peerCredOSSupport marks platforms where kernel-level peer credentials are
// available. Elsewhere there is no single-call equivalent of real's
// Bun.ant.getPeerPid/getPeerUid — peers are unverifiable at the kernel level
// and are handled by the hold/accept rules in services.go.
const peerCredOSSupport = false

func peerCredsOf(net.Conn) (pid, uid, gid int, ok bool) {
	return -1, -1, -1, false
}

// statUID: no syscall.Stat_t off posix — ownership checks degrade to
// "can't verify" (vets treat it as non-foreign, matching the old leaf-only
// behavior on platforms that lack the stat).
func statUID(os.FileInfo) (int, bool) {
	return 0, false
}
