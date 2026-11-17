// Package hostmem reads the host's total physical memory.
package hostmem

// DefaultReserve is the default reserve line: the larger of 15% of total
// memory and 8GiB, capped at half of total so small machines keep capacity.
func DefaultReserve(total int64) int64 {
	return min(max(total*15/100, 8<<30), total/2)
}

// MonitorReserve is the default reserve line where memory limits are only
// monitored (macOS): the larger of 25% of total memory and 8GiB, capped at
// half of total. With no kernel limit a lease overshoots its own by what
// it allocates before the next check. Measured on macOS with a check
// every second: 33MiB for a lease allocating 100MiB a second, 1GiB for
// one allocating 2GiB a second; with the check every 200ms the daemon now
// makes, 8MiB and 284MiB. The extra reserve absorbs that for a few leases
// at once.
func MonitorReserve(total int64) int64 {
	return min(max(total*25/100, 8<<30), total/2)
}
