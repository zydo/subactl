package hostmem

import "golang.org/x/sys/unix"

// Total returns hw.memsize in bytes.
func Total() (int64, error) {
	n, err := unix.SysctlUint64("hw.memsize")
	return int64(n), err
}
