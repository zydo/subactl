package capacity

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// DarwinReader reads the whole machine on macOS. Available memory is what
// the kernel's memory-pressure machinery considers free, the same figure
// "memory_pressure" prints as "System-wide memory free percentage": free
// pages plus what it can reclaim without swapping.
type DarwinReader struct{}

// Read returns hw.memsize and that share of it.
func (DarwinReader) Read() (Host, error) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return Host{}, fmt.Errorf("capacity: hw.memsize: %w", err)
	}
	level, err := unix.SysctlUint32("kern.memorystatus_level")
	if err != nil {
		return Host{}, fmt.Errorf("capacity: kern.memorystatus_level: %w", err)
	}
	if level > 100 {
		level = 100
	}
	return Host{Total: int64(total), Available: int64(total) * int64(level) / 100}, nil
}
