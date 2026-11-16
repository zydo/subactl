package main

import (
	"strings"
	"testing"
	"time"

	"github.com/zydo/subactl/api"
)

func TestRenderTop(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	gib := int64(1) << 30
	lease := func(id, parent, kind string, used, limit int64, kids ...api.LeaseID) api.Lease {
		return api.Lease{ID: api.LeaseID(id), Parent: api.LeaseID(parent), Kind: kind, State: api.StateRunning,
			Envelope: api.Envelope{Memory: limit}, Usage: &api.Usage{Memory: used, Pids: 12},
			CreatedAt: now.Add(-90 * time.Second), Children: kids}
	}
	a1 := lease("ls_a1", "ls_a", "upgrade", 19*gib/10, 2*gib, "ls_a1x")
	a1.Activity = &api.Activity{State: "working", Tool: "bash", Seq: 3, At: now.Add(-2 * time.Second)}
	a2 := lease("ls_a2", "ls_a", "upgrade", gib, 2*gib)
	a2.Activity = &api.Activity{State: "working", Tool: "bash", Seq: 9, At: now.Add(-12 * time.Minute)}
	gone := lease("ls_old", "ls_a", "fix", 0, gib)
	gone.State = api.StateReaped
	v := topView{
		Now:  now,
		Host: api.Host{Backend: "cgroup2", MemBasis: api.BasisWorkingSet, EffectiveCapacity: 20 * gib, Allocated: 9 * gib, External: gib, Reserve: 8 * gib, CapacityShrunk: true},
		Leases: []api.Lease{
			lease("ls_a", "", "lead", 3*gib, 9*gib, "ls_a1", "ls_old", "ls_a2"),
			a1, lease("ls_a1x", "ls_a1", "fix", gib/4, gib/2), gone, a2,
			lease("ls_b", "", "lead", 0, 4*gib),
		},
		Events: []api.Event{{Type: "TERMINATED", Lease: "ls_old", At: now, Data: map[string]any{"code": 413.0, "name": "OOM_LEASE_LIMIT"}}},
	}
	out := strings.Join(renderTop(v), "\n")
	for _, want := range []string{
		"capacity 20GiB", "allocated 9GiB (45%)", "external 1GiB", "CAPACITY SHRUNK",
		"ls_a ", "├─ ls_a1 ", "│  └─ ls_a1x", "└─ ls_a2",
		"[!!!!!!!!!.]",                      // 95% of the limit: past the warning line
		"[#####.....]",                      // 50%
		"working (bash #3)",                 // the tool, never its arguments
		"working (bash #9), silent 12m00s",  // a stall is visible
		"1m30s",                             // age
		"TERMINATED", "413 OOM_LEASE_LIMIT", // recent events
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ls_old   ") && strings.Contains(out, "reaped") {
		t.Errorf("a reaped lease is listed among the live ones:\n%s", out)
	}
	// Narrow terminals clip, never wrap.
	v.Width = 40
	for _, line := range renderTop(v) {
		if len([]rune(line)) > 40 {
			t.Errorf("line of %d columns in a 40-column view: %q", len([]rune(line)), line)
		}
	}
	if got := renderTop(topView{Now: now}); !strings.Contains(strings.Join(got, "\n"), "(no live leases)") {
		t.Errorf("empty view: %v", got)
	}
}
