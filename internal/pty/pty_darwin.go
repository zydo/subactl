package pty

import (
	"bytes"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Open allocates a pseudo-terminal.
func Open() (*Pair, error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	fd := m.Fd()
	ioctl := func(req uint, arg unsafe.Pointer) error {
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(req), uintptr(arg)); e != 0 {
			return e
		}
		return nil
	}
	// grantpt, unlockpt and ptsname, as ioctls.
	if err := ioctl(unix.TIOCPTYGRANT, nil); err != nil {
		m.Close()
		return nil, fmt.Errorf("pty: grant: %w", err)
	}
	if err := ioctl(unix.TIOCPTYUNLK, nil); err != nil {
		m.Close()
		return nil, fmt.Errorf("pty: unlock: %w", err)
	}
	var name [128]byte
	if err := ioctl(unix.TIOCPTYGNAME, unsafe.Pointer(&name[0])); err != nil {
		m.Close()
		return nil, fmt.Errorf("pty: slave name: %w", err)
	}
	return &Pair{Master: m, Slave: string(name[:bytes.IndexByte(name[:], 0)])}, nil
}
