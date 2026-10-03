//go:build darwin

package tools

import (
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// peerCredOSSupport marks platforms where kernel-level peer credentials are
// available (real's Bun.ant.getPeerPid / getPeerUid / getPeerGid).
const peerCredOSSupport = true

// peerCredsOf returns the connecting peer's pid/uid/gid on darwin:
// getsockopt(SOL_LOCAL, LOCAL_PEERPID=0x2) for the pid; LOCAL_PEERUID (0x8 —
// the opt real's getPeerUid uses) for the uid, with a LOCAL_PEERCRED xucred
// fallback on kernels where 0x8 is unsupported.
func peerCredsOf(c net.Conn) (pid, uid, gid int, ok bool) {
	pid, uid, gid = -1, -1, -1
	uc, is := c.(*net.UnixConn)
	if !is {
		return
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return
	}
	// SOL_LOCAL=0x0 — literals mirror real's option numbers.
	raw.Control(func(fd uintptr) {
		if p, err := unix.GetsockoptInt(int(fd), 0x0, 0x2); err == nil { // LOCAL_PEERPID
			pid = p
		}
		if u, err := unix.GetsockoptInt(int(fd), 0x0, 0x8); err == nil { // LOCAL_PEERUID
			uid = u
		}
		if x, err := unix.GetsockoptXucred(int(fd), 0x0, 0x1); err == nil { // LOCAL_PEERCRED
			if uid < 0 {
				uid = int(x.Uid)
			}
			if x.Ngroups > 0 {
				gid = int(x.Groups[0])
			}
		}
	})
	ok = pid > 0 || uid >= 0
	return
}

// statUID extracts the owner uid from a FileInfo's syscall stat.
func statUID(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
