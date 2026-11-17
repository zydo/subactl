package capacity

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// The reading agrees with what memory_pressure reports.
func TestDarwinReader(t *testing.T) {
	h, err := DarwinReader{}.Read()
	if err != nil {
		t.Fatal(err)
	}
	if h.Total < 1<<30 || h.Available <= 0 || h.Available > h.Total {
		t.Fatalf("reading %+v", h)
	}
	out, err := exec.Command("memory_pressure").Output()
	if err != nil {
		t.Skipf("memory_pressure is not available: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if pct, ok := strings.CutPrefix(line, "System-wide memory free percentage: "); ok {
			want, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(pct), "%"))
			got := int(100 * h.Available / h.Total)
			if d := got - want; d < -5 || d > 5 {
				t.Fatalf("available %d%% of total, memory_pressure says %d%%", got, want)
			}
			return
		}
	}
	t.Fatal("memory_pressure printed no free percentage")
}
