package bench

import (
	"math"
	"testing"
	"time"
)

func TestSummarize(t *testing.T) {
	s := &Summary{ChamberBytes: 8 << 30}
	Summarize(s, []Sample{
		{T: 0, WS: 1 << 30, AgentsWS: 1 << 30},
		{T: 1, WS: 6 << 30, AgentsWS: 4 << 30, OOMKills: 20, CapHits: 2},
		{T: 2, WS: 3 << 30, AgentsWS: 1 << 30, OOMKills: 31, CapHits: 3},
	})
	if s.HostPeakWS != 6 || s.HostOOM != 3 || s.OOMKills != 31 {
		t.Fatalf("peak %v, cap hits %d, kills %d", s.HostPeakWS, s.HostOOM, s.OOMKills)
	}
	// Mean agent working set is 2GiB of an 8GiB chamber.
	if math.Abs(s.UtilMean-0.25) > 1e-9 {
		t.Fatalf("util_mean %v", s.UtilMean)
	}
	empty := &Summary{ChamberBytes: 8 << 30}
	Summarize(empty, nil)
	if empty.UtilMean != 0 || empty.HostPeakWS != 0 {
		t.Fatalf("empty series: %+v", empty)
	}
}

func TestHogHoldsThenReturns(t *testing.T) {
	start := time.Now()
	Hog(64<<20, 100*time.Millisecond, 100*time.Millisecond)
	if d := time.Since(start); d < 200*time.Millisecond || d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
}
