//go:build linux

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

// peerCredsOf returns the connecting peer's pid/uid/gid via
// getsockopt(SOL_SOCKET, SO_PEERCRED) — the linux equivalent of real's
// Bun.ant credential reads.
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
	var cred *unix.Ucred
	var gerr error
	raw.Control(func(fd uintptr) {
		cred, gerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if gerr != nil || cred == nil {
		return
	}
	return int(cred.Pid), int(cred.Uid), int(cred.Gid), true
}

// statUID extracts the owner uid from a FileInfo's syscall stat.
func statUID(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
