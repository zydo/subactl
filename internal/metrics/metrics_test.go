package metrics

import (
	"strings"
	"testing"
)

func TestWrite(t *testing.T) {
	m := New()
	m.Termination(200, "worker", 100<<20)
	m.Termination(413, "worker", 3<<30)
	m.Termination(503, "", 1<<40) // above the last bucket, no kind
	m.Reject("mem_budget")
	m.Reject("mem_budget")
	m.SafetyNet()
	var b strings.Builder
	m.Write(&b, Gauges{EffectiveCapacity: 100, Allocated: 60, External: 7, Reserve: 8, CapacityShrunk: true, LiveByDepth: map[int]int{0: 2, 1: 5}, DroppedAudit: 3})
	out := b.String()
	for _, want := range []string{
		"subactl_effective_capacity_bytes 100\n",
		"subactl_capacity_shrunk 1\n",
		`subactl_live_leases{depth="1"} 5`,
		`subactl_terminations_total{code="413"} 1`,
		"subactl_safety_net_total 1\n",
		`subactl_admission_rejections_total{reason="mem_budget"} 2`,
		`subactl_lease_peak_bytes_bucket{kind="worker",le="134217728"} 1`,
		`subactl_lease_peak_bytes_bucket{kind="worker",le="+Inf"} 2`,
		`subactl_lease_peak_bytes_count{kind="worker"} 2`,
		`subactl_lease_peak_bytes_bucket{kind="none",le="68719476736"} 0`,
		`subactl_lease_peak_bytes_bucket{kind="none",le="+Inf"} 1`,
		`subactl_telemetry_dropped_total{sink="audit"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Every sample line is "name{labels} value" or "name value".
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(line, "#") && len(strings.Fields(line)) != 2 {
			t.Errorf("malformed line %q", line)
		}
	}
}

// F30: the number of series does not grow with the number of leases.
func TestSeriesDoNotGrowWithLeases(t *testing.T) {
	count := func(m *Metrics) int {
		var b strings.Builder
		m.Write(&b, Gauges{LiveByDepth: map[int]int{0: 1}})
		n := 0
		for _, line := range strings.Split(b.String(), "\n") {
			if line != "" && !strings.HasPrefix(line, "#") {
				n++
			}
		}
		return n
	}
	m := New()
	for i := 0; i < 100; i++ {
		m.Termination(200, "worker", int64(i)<<20)
	}
	before := count(m)
	for i := 0; i < 10000; i++ {
		m.Termination(200, "worker", int64(i)<<20)
	}
	if after := count(m); after != before {
		t.Fatalf("series grew from %d to %d over 10000 leases", before, after)
	}
}
