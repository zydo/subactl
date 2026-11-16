// Package metrics writes subactld's metrics in the Prometheus text format,
// by hand, with no client library.
//
// Labels are kind, depth, code and reason only. Lease and lead ids are
// never labels: leases come and go without end, and a series per lease
// would grow without bound. Per-lease data is in the trace instead.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
)

// buckets are the upper bounds of the peak-usage histogram, in bytes:
// 64MiB to 64GiB by powers of two.
var buckets = func() []int64 {
	var b []int64
	for v := int64(64 << 20); v <= 64<<30; v *= 2 {
		b = append(b, v)
	}
	return b
}()

type hist struct {
	counts []int64 // one per bucket, not cumulative
	over   int64   // above the last bucket
	sum    int64
}

// Gauges are the values read at scrape time.
type Gauges struct {
	EffectiveCapacity, Allocated, External, Reserve int64
	CapacityShrunk                                  bool
	LiveByDepth                                     map[int]int
	DroppedEvents, DroppedAudit                     int64
}

// Metrics accumulates counters. It is safe for concurrent use.
type Metrics struct {
	mu        sync.Mutex
	codes     map[int]int64
	rejects   map[string]int64
	safetyNet int64
	peaks     map[string]*hist // by kind
}

// New returns empty metrics.
func New() *Metrics {
	return &Metrics{codes: map[int]int64{}, rejects: map[string]int64{}, peaks: map[string]*hist{}}
}

// Termination counts one ended lease and records its peak memory.
func (m *Metrics) Termination(code int, kind string, peak int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.codes[code]++
	if kind == "" {
		kind = "none"
	}
	h := m.peaks[kind]
	if h == nil {
		h = &hist{counts: make([]int64, len(buckets))}
		m.peaks[kind] = h
	}
	h.sum += peak
	for i, b := range buckets {
		if peak <= b {
			h.counts[i]++
			return
		}
	}
	h.over++
}

// Reject counts one refused admission or resize.
func (m *Metrics) Reject(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rejects[reason]++
}

// SafetyNet counts one safety-net termination.
func (m *Metrics) SafetyNet() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.safetyNet++
}

// SafetyNetCount returns the number of safety-net terminations so far.
func (m *Metrics) SafetyNetCount() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.safetyNet
}

// Write renders everything in the Prometheus text exposition format.
func (m *Metrics) Write(w io.Writer, g Gauges) {
	m.mu.Lock()
	defer m.mu.Unlock()
	gauge := func(name, help string, v int64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, v)
	}
	gauge("subactl_effective_capacity_bytes", "Memory that top-level leases may be given in total.", g.EffectiveCapacity)
	gauge("subactl_allocated_bytes", "Sum of the memory hard limits of live top-level leases.", g.Allocated)
	gauge("subactl_external_bytes", "Memory used by processes subactld does not manage.", g.External)
	gauge("subactl_reserve_bytes", "The reserve line kept for the system.", g.Reserve)
	shrunk := int64(0)
	if g.CapacityShrunk {
		shrunk = 1
	}
	gauge("subactl_capacity_shrunk", "1 while allocation exceeds the effective capacity.", shrunk)

	fmt.Fprint(w, "# HELP subactl_live_leases Leases that have not been reaped, by depth in the tree.\n# TYPE subactl_live_leases gauge\n")
	depths := make([]int, 0, len(g.LiveByDepth))
	for d := range g.LiveByDepth {
		depths = append(depths, d)
	}
	sort.Ints(depths)
	for _, d := range depths {
		fmt.Fprintf(w, "subactl_live_leases{depth=\"%d\"} %d\n", d, g.LiveByDepth[d])
	}

	fmt.Fprint(w, "# HELP subactl_terminations_total Ended leases by termination code.\n# TYPE subactl_terminations_total counter\n")
	codes := make([]int, 0, len(m.codes))
	for c := range m.codes {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	for _, c := range codes {
		fmt.Fprintf(w, "subactl_terminations_total{code=\"%d\"} %d\n", c, m.codes[c])
	}

	fmt.Fprintf(w, "# HELP subactl_safety_net_total Leases terminated by the safety net.\n# TYPE subactl_safety_net_total counter\nsubactl_safety_net_total %d\n", m.safetyNet)

	fmt.Fprint(w, "# HELP subactl_admission_rejections_total Refused acquires and resizes by reason.\n# TYPE subactl_admission_rejections_total counter\n")
	reasons := make([]string, 0, len(m.rejects))
	for r := range m.rejects {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		fmt.Fprintf(w, "subactl_admission_rejections_total{reason=%s} %d\n", strconv.Quote(r), m.rejects[r])
	}

	fmt.Fprint(w, "# HELP subactl_lease_peak_bytes Peak memory of ended leases by kind.\n# TYPE subactl_lease_peak_bytes histogram\n")
	kinds := make([]string, 0, len(m.peaks))
	for k := range m.peaks {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		h, q := m.peaks[k], strconv.Quote(k)
		var cum int64
		for i, b := range buckets {
			cum += h.counts[i]
			fmt.Fprintf(w, "subactl_lease_peak_bytes_bucket{kind=%s,le=\"%d\"} %d\n", q, b, cum)
		}
		cum += h.over
		fmt.Fprintf(w, "subactl_lease_peak_bytes_bucket{kind=%s,le=\"+Inf\"} %d\n", q, cum)
		fmt.Fprintf(w, "subactl_lease_peak_bytes_sum{kind=%s} %d\n", q, h.sum)
		fmt.Fprintf(w, "subactl_lease_peak_bytes_count{kind=%s} %d\n", q, cum)
	}

	fmt.Fprint(w, "# HELP subactl_telemetry_dropped_total Observations dropped because a buffer was full or a write failed.\n# TYPE subactl_telemetry_dropped_total counter\n")
	fmt.Fprintf(w, "subactl_telemetry_dropped_total{sink=\"events\"} %d\n", g.DroppedEvents)
	fmt.Fprintf(w, "subactl_telemetry_dropped_total{sink=\"audit\"} %d\n", g.DroppedAudit)
}
