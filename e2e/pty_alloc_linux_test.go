//go:build e2e && linux

package e2e

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ptyAlloc: /dev/ptmx + TIOCGPTN/TIOCSPTLCK → /dev/pts/<n> (Linux).
func ptyAlloc(cols, rows int) (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var n uint32
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(),
		uintptr(0x80045430 /* TIOCGPTN */), uintptr(unsafe.Pointer(&n))); errno != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("ioctl TIOCGPTN: %w", errno)
	}
	var zero uint32
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(),
		uintptr(0x40045431 /* TIOCSPTLCK */), uintptr(unsafe.Pointer(&zero))); errno != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("ioctl TIOCSPTLCK: %w", errno)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("open /dev/pts/%d: %w", n, err)
	}
	if cols > 0 && rows > 0 {
		_ = unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ,
			&unix.Winsize{Row: uint16(rows), Col: uint16(cols)})
	}
	return master, slave, nil
}

func ptyProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
