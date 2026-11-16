package telemetry

import (
	"testing"
	"time"

	"github.com/zydo/subactl/api"
)

func TestBusNeverBlocksAndCountsDrops(t *testing.T) {
	b := NewBus()
	slow := b.Subscribe(2)
	fast := b.Subscribe(100)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			b.Publish(Scoped{Event: api.Event{Type: EventState}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a full subscriber")
	}
	if len(slow.C) != 2 || len(fast.C) != 50 || b.Dropped() != 48 {
		t.Fatalf("slow %d, fast %d, dropped %d", len(slow.C), len(fast.C), b.Dropped())
	}
	if e := <-fast.C; e.Seq != 1 || e.At.IsZero() {
		t.Fatalf("first event %+v", e)
	}
	b.Unsubscribe(slow)
	b.Publish(Scoped{})
	if b.Dropped() != 48 {
		t.Fatal("an unsubscribed subscriber still counts")
	}
}

func TestVisible(t *testing.T) {
	e := Scoped{Path: []api.LeaseID{"kid", "mid", "lead"}, OwnerUID: 501}
	for _, tc := range []struct {
		node  api.LeaseID
		uid   int
		admin bool
		want  bool
	}{
		{"kid", 0, false, true}, {"mid", 0, false, true}, {"lead", 0, false, true},
		{"sibling", 0, false, false},
		{"", 501, false, true}, {"", 502, false, false}, {"", 502, true, true},
	} {
		if got := e.Visible(tc.node, tc.uid, tc.admin); got != tc.want {
			t.Errorf("Visible(%q, %d, %v) = %v", tc.node, tc.uid, tc.admin, got)
		}
	}
	if !(Scoped{}).Visible("anyone", 1, false) {
		t.Error("a host event must be visible to everyone")
	}
}

func TestRing(t *testing.T) {
	r := NewRing(4)
	t0 := time.Unix(1000, 0)
	for i := 0; i < 6; i++ {
		r.Add(api.Usage{Memory: int64(i) * 100, SampledAt: t0.Add(time.Duration(i) * time.Second)})
	}
	s := r.Samples()
	if len(s) != 4 || s[0].Memory != 200 || s[3].Memory != 500 {
		t.Fatalf("samples %+v", s)
	}
	if rate := r.Rate(); rate != 100 {
		t.Fatalf("rate %v, want 100 bytes/s", rate)
	}
	if NewRing(4).Rate() != 0 {
		t.Fatal("rate of an empty ring")
	}
}
