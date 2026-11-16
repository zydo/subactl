package pty

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Open allocates a pseudo-terminal.
func Open() (*Pair, error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	fd := int(m.Fd())
	// Unlock the slave, then ask for its number.
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		m.Close()
		return nil, fmt.Errorf("pty: unlock: %w", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		m.Close()
		return nil, fmt.Errorf("pty: slave number: %w", err)
	}
	return &Pair{Master: m, Slave: fmt.Sprintf("/dev/pts/%d", n)}, nil
}
