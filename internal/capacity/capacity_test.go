package capacity

import (
	"testing"

	"github.com/zydo/subactl/api"
)

const gib = int64(1) << 30

// F5: effective capacity does not move with the agents' own usage.
func TestEffectiveIgnoresManagedUsage(t *testing.T) {
	const total, reserve = 64 * gib, 8 * gib
	// 10GiB of other processes throughout; the agents use 0, then 30GiB.
	idle := Host{Total: total, Available: total - 10*gib}
	busy := Host{Total: total, Available: total - 10*gib - 30*gib}
	a := Effective(0, idle, reserve, External(idle, 0))
	b := Effective(0, busy, reserve, External(busy, 30*gib))
	if a != 46*gib || b != a {
		t.Fatalf("effective capacity moved from %d to %d with agent usage", a, b)
	}
	// External load does move it.
	loaded := Host{Total: total, Available: total - 25*gib}
	if got := Effective(0, loaded, reserve, External(loaded, 0)); got != 31*gib {
		t.Fatalf("got %d", got)
	}
	if got := Effective(20*gib, idle, reserve, External(idle, 0)); got != 20*gib {
		t.Fatalf("the configured cap was ignored: %d", got)
	}
	if got := Effective(0, Host{Total: 4 * gib, Available: gib}, reserve, 0); got != 0 {
		t.Fatalf("capacity went negative: %d", got)
	}
}

func TestNetHysteresis(t *testing.T) {
	n := Net{Need: 3}
	readings := []struct {
		avail int64
		want  bool
	}{{7, false}, {7, false}, {9, false}, {7, false}, {7, false}, {7, true}, {7, true}, {8, false}, {7, false}}
	for i, r := range readings {
		if got := n.Observe(r.avail*gib, 8*gib); got != r.want {
			t.Fatalf("reading %d (%dGiB): tripped=%v, want %v", i, r.avail, got, r.want)
		}
	}
}

func TestVictim(t *testing.T) {
	k := func(n int) *int { return &n }
	node := func(id, parent, lead string, usage int64, leaf bool, order *int) Node {
		return Node{ID: api.LeaseID(id), Parent: api.LeaseID(parent), Lead: api.LeaseID(lead), Usage: usage * gib, Leaf: leaf, KillOrder: order}
	}
	base := []Node{
		node("A", "", "A", 10, false, nil),
		node("a1", "A", "A", 6, false, nil),
		node("a1x", "a1", "A", 2, true, nil),
		node("a1y", "a1", "A", 3, true, nil),
		node("a2", "A", "A", 1, true, nil),
		node("B", "", "B", 4, false, nil),
		node("b1", "B", "B", 3, true, k(1)),
	}
	// The largest lead is A; its largest leaf is a1y. B's hint plays no part.
	if got, _ := Victim(base); got != "a1y" {
		t.Fatalf("got %s, want a1y", got)
	}
	// A hint inside A wins over the default, lowest first.
	hinted := append([]Node{}, base...)
	hinted[4].KillOrder = k(2) // a2
	hinted[1].KillOrder = k(5) // a1
	if got, _ := Victim(hinted); got != "a2" {
		t.Fatalf("got %s, want a2 (kill_order 2)", got)
	}
	// A lead with no nested lease is its own leaf.
	if got, _ := Victim([]Node{node("solo", "", "solo", 1, true, nil)}); got != "solo" {
		t.Fatalf("got %s", got)
	}
	if _, ok := Victim(nil); ok {
		t.Fatal("picked a victim from nothing")
	}
}
