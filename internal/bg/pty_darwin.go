//go:build darwin

package bg

import (
	"bytes"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
	"unsafe"
)

// openPTY allocates a master/slave pair — /dev/ptmx + TIOCPTYGNAME (Darwin
// auto-grants/unlocks the slave on master open; grant/unlock ioctls run
// anyway for parity with the e2e harness).
func openPTY(cols, rows int) (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var buf [128]byte
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(),
		uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("ioctl TIOCPTYGNAME: %w", errno)
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(),
		uintptr(unix.TIOCPTYGRANT), 0); errno != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("ioctl TIOCPTYGRANT: %w", errno)
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(),
		uintptr(unix.TIOCPTYUNLK), 0); errno != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("ioctl TIOCPTYUNLK: %w", errno)
	}
	name := string(buf[:bytes.IndexByte(buf[:], 0)])
	slave, err = os.OpenFile(name, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("open pty slave %s: %w", name, err)
	}
	if cols > 0 && rows > 0 {
		_ = unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ,
			&unix.Winsize{Row: uint16(rows), Col: uint16(cols)})
	}
	return master, slave, nil
}

// setWinSize pushes a new size onto the slave side of a pty.
func setWinSize(f *os.File, cols, rows int) {
	_ = unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ,
		&unix.Winsize{Row: uint16(rows), Col: uint16(cols)})
}
