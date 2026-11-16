// Package hostmem reads the host's total physical memory.
package hostmem

// DefaultReserve is the default reserve line: the larger of 15% of total
// memory and 8GiB, capped at half of total so small machines keep capacity.
func DefaultReserve(total int64) int64 {
	return min(max(total*15/100, 8<<30), total/2)
}
