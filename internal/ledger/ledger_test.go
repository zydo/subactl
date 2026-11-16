package ledger

import (
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/zydo/subactl/api"
)

const gib = int64(1) << 30

func env(mem, self int64) api.Envelope { return api.Envelope{Memory: mem, MemorySelf: self} }

func wantReason(t *testing.T, err error, reason string) {
	t.Helper()
	var r *Rejection
	if !errors.As(err, &r) || r.Reason != reason {
		t.Fatalf("got %v, want rejection %s", err, reason)
	}
}

func mustAcquire(t *testing.T, l *Ledger, id, parent api.LeaseID, e api.Envelope) {
	t.Helper()
	if _, err := l.Acquire(Params{ID: id, Parent: parent, Envelope: e}); err != nil {
		t.Fatalf("acquire %s: %v", id, err)
	}
	if err := l.Running(id); err != nil {
		t.Fatal(err)
	}
}

func reap(t *testing.T, l *Ledger, id api.LeaseID, code int) {
	t.Helper()
	for _, m := range l.BeginReap(id, code) {
		if err := l.Finish(m.ID, api.Termination{Code: m.Code}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTopLevelNoOversell(t *testing.T) {
	l := New(10 * gib)
	mustAcquire(t, l, "a", "", env(6*gib, 0))
	_, err := l.Acquire(Params{ID: "b", Envelope: env(5*gib, 0)})
	wantReason(t, err, api.ReasonMemBudget)
	mustAcquire(t, l, "b", "", env(4*gib, 0)) // exactly full is allowed
	_, err = l.Acquire(Params{ID: "c", Envelope: env(1, 0)})
	wantReason(t, err, api.ReasonMemBudget)

	// A reservation is held through reaping and released only at reaped.
	l.BeginReap("a", api.CodeKilled)
	_, err = l.Acquire(Params{ID: "c", Envelope: env(gib, 0)})
	wantReason(t, err, api.ReasonMemBudget)
	if err := l.Finish("a", api.Termination{Code: api.CodeKilled}); err != nil {
		t.Fatal(err)
	}
	mustAcquire(t, l, "c", "", env(6*gib, 0))
	if _, alloc, _ := l.Totals(); alloc != 10*gib {
		t.Fatalf("allocated %d", alloc)
	}
}

func TestNestedAdmission(t *testing.T) {
	l := New(100 * gib)
	mustAcquire(t, l, "lead", "", env(10*gib, 2*gib))
	mustAcquire(t, l, "w1", "lead", env(5*gib, 0))
	// 5 (sibling) + 4 (request) + 2 (memory_self) > 10
	_, err := l.Acquire(Params{ID: "w2", Parent: "lead", Envelope: env(4*gib, 0)})
	wantReason(t, err, api.ReasonParentBudget)
	mustAcquire(t, l, "w2", "lead", env(3*gib, 0))
	// Nested limits are inside the parent: the top-level sum is unchanged.
	if _, alloc, _ := l.Totals(); alloc != 10*gib {
		t.Fatalf("allocated %d, want the lead's 10GiB only", alloc)
	}
	// A parent that is not running cannot have children.
	if _, err := l.Acquire(Params{ID: "adm", Envelope: env(gib, 0)}); err != nil {
		t.Fatal(err)
	}
	_, err = l.Acquire(Params{ID: "x", Parent: "adm", Envelope: env(1, 0)})
	wantReason(t, err, api.ReasonParentNotRunning)
	_, err = l.Acquire(Params{ID: "x", Parent: "nope", Envelope: env(1, 0)})
	wantReason(t, err, api.ReasonParentNotRunning)
	if a, _ := l.Available("lead"); a.MemoryFree != 0 || a.LiveChildren != 2 || a.MemoryAllocated != 8*gib {
		t.Fatalf("available %+v", a)
	}
}

func TestDryRunReservesNothing(t *testing.T) {
	l := New(4 * gib)
	if _, err := l.Acquire(Params{ID: "a", Envelope: env(3*gib, 0), DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Get("a"); ok {
		t.Fatal("dry run created a lease")
	}
	_, err := l.Acquire(Params{ID: "a", Envelope: env(5*gib, 0), DryRun: true})
	wantReason(t, err, api.ReasonMemBudget)
}

func TestCapacityShrunk(t *testing.T) {
	l := New(10 * gib)
	mustAcquire(t, l, "a", "", env(8*gib, 0))
	mustAcquire(t, l, "kid", "a", env(gib, 0))
	if shrunk := l.SetEffectiveCapacity(6 * gib); !shrunk {
		t.Fatal("want shrunk")
	}
	// Nothing is revoked, but nothing new is granted either, even nested.
	_, err := l.Acquire(Params{ID: "b", Envelope: env(1, 0)})
	wantReason(t, err, api.ReasonCapacityShrunk)
	_, err = l.Acquire(Params{ID: "kid2", Parent: "a", Envelope: env(gib, 0)})
	wantReason(t, err, api.ReasonCapacityShrunk)
	up := 2 * gib
	_, err = l.Resize("kid", api.EnvelopePatch{Memory: &up}, 0)
	wantReason(t, err, api.ReasonCapacityShrunk)
	// Lowering is still allowed and can end the shrink.
	down := 5 * gib
	if _, err := l.Resize("a", api.EnvelopePatch{Memory: &down}, gib); err != nil {
		t.Fatal(err)
	}
	if _, _, shrunk := l.Totals(); shrunk {
		t.Fatal("still shrunk after lowering below capacity")
	}
	mustAcquire(t, l, "b", "", env(gib, 0))
}

func TestResize(t *testing.T) {
	l := New(10 * gib)
	mustAcquire(t, l, "a", "", env(4*gib, gib))
	mustAcquire(t, l, "b", "", env(4*gib, 0))
	mustAcquire(t, l, "a1", "a", env(gib, 0))
	mustAcquire(t, l, "a2", "a", env(gib, 0))
	p := func(v int64) api.EnvelopePatch { return api.EnvelopePatch{Memory: &v} }

	_, err := l.Resize("a", p(7*gib), 0) // 7 + 4 > 10
	wantReason(t, err, api.ReasonMemBudget)
	if _, err := l.Resize("a", p(6*gib), 0); err != nil {
		t.Fatal(err)
	}
	_, err = l.Resize("a1", p(5*gib), 0) // 5 + 1 (a2) + 1 (self) > 6
	wantReason(t, err, api.ReasonParentBudget)
	if _, err := l.Resize("a1", p(4*gib), 0); err != nil {
		t.Fatal(err)
	}

	// Lowering: floor is usage + max(10%, 64MiB).
	_, err = l.Resize("b", p(2*gib), 2*gib)
	wantReason(t, err, api.ReasonBelowUsage)
	_, err = l.Resize("b", p(2*gib+MinHeadroom-1), 2*gib-MinHeadroom/2)
	wantReason(t, err, api.ReasonBelowUsage) // 10% of ~2GiB is ~205MiB
	if _, err := l.Resize("b", p(gib+MinHeadroom), gib/2); err != nil {
		t.Fatal(err)
	}
	// Lowering below what the children hold.
	_, err = l.Resize("a", p(5*gib), 0) // self 1 + a1 4 + a2 1 = 6
	wantReason(t, err, api.ReasonBelowAllocated)
	self := 2 * gib
	_, err = l.Resize("a", api.EnvelopePatch{MemorySelf: &self}, 0)
	wantReason(t, err, api.ReasonBelowAllocated)

	// Dry run changes nothing.
	if got, err := l.Resize("b", api.EnvelopePatch{Memory: ptr(3 * gib), DryRun: true}, 0); err != nil || got.Envelope.Memory != 3*gib {
		t.Fatal(got, err)
	}
	if got, _ := l.Get("b"); got.Envelope.Memory != gib+MinHeadroom {
		t.Fatalf("dry run changed the limit to %d", got.Envelope.Memory)
	}
	if err := l.Check(); err != nil {
		t.Fatal(err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestReapCascadeAndFirstCodeWins(t *testing.T) {
	l := New(10 * gib)
	mustAcquire(t, l, "lead", "", env(8*gib, 0))
	mustAcquire(t, l, "w", "lead", env(4*gib, 0))
	mustAcquire(t, l, "ww", "w", env(gib, 0))

	// The grandchild dies on its own first and keeps its own code.
	if got := l.BeginReap("ww", api.CodeOOMLeaseLimit); len(got) != 1 {
		t.Fatalf("marked %v", got)
	}
	got := l.BeginReap("lead", api.CodeExternalAbort)
	want := []Marked{{"w", api.CodeParentDied}, {"lead", api.CodeExternalAbort}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("marked %v, want %v (children first)", got, want)
	}
	if got := l.BeginReap("lead", api.CodeKilled); got != nil {
		t.Fatalf("second BeginReap marked %v", got)
	}
	if l.Code("lead") != api.CodeExternalAbort || l.Code("ww") != api.CodeOOMLeaseLimit {
		t.Fatal("first code did not win")
	}
	// A parent cannot finish before its children.
	if err := l.Finish("w", api.Termination{}); err == nil {
		t.Fatal("finished a lease with a live child")
	}
	for _, id := range []api.LeaseID{"ww", "w", "lead"} {
		if err := l.Finish(id, api.Termination{Code: l.Code(id)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, alloc, _ := l.Totals(); alloc != 0 {
		t.Fatalf("allocated %d after everything was reaped", alloc)
	}
	// Reaped leases stay queryable with their record.
	if ls, ok := l.Get("ww"); !ok || ls.State != api.StateReaped || ls.Termination.Code != api.CodeOOMLeaseLimit {
		t.Fatalf("got %+v", ls)
	}
}

func TestReapedHistoryIsBounded(t *testing.T) {
	l := New(10 * gib)
	mustAcquire(t, l, "lead", "", env(8*gib, 0))
	for i := 0; i < keptReaped+50; i++ {
		id := api.LeaseID(fmt.Sprintf("w%d", i))
		mustAcquire(t, l, id, "lead", env(gib, 0))
		reap(t, l, id, api.CodeCompleted)
	}
	if n := len(l.Subtree("lead")); n != keptReaped+1 {
		t.Fatalf("kept %d leases, want %d", n, keptReaped+1)
	}
	if _, ok := l.Get("w0"); ok {
		t.Fatal("oldest reaped child was not dropped")
	}
	if err := l.Check(); err != nil {
		t.Fatal(err)
	}
}

// TestRandomOperations drives the ledger with random operations and checks
// after every step that no reservation was granted beyond capacity (I-B)
// and that the tree invariants hold.
func TestRandomOperations(t *testing.T) {
	for seed := int64(1); seed <= 20; seed++ {
		rng := rand.New(rand.NewSource(seed))
		capacity := 64 * gib
		l := New(capacity)
		var live []api.LeaseID
		next := 0
		for step := 0; step < 3000; step++ {
			pick := func() api.LeaseID { return live[rng.Intn(len(live))] }
			_, before, _ := l.Totals()
			granted := false
			switch op := rng.Intn(10); {
			case op < 4 || len(live) == 0: // acquire
				next++
				id := api.LeaseID(fmt.Sprintf("l%d", next))
				var parent api.LeaseID
				if len(live) > 0 && rng.Intn(3) > 0 {
					parent = pick()
				}
				mem := rng.Int63n(16*gib) + 1
				if _, err := l.Acquire(Params{ID: id, Parent: parent, Envelope: env(mem, rng.Int63n(mem+1))}); err == nil {
					granted = true
					if rng.Intn(5) > 0 {
						_ = l.Running(id)
					}
					live = append(live, id)
				}
			case op < 6: // resize
				mem := rng.Int63n(20*gib) + 1
				if _, err := l.Resize(pick(), api.EnvelopePatch{Memory: &mem}, rng.Int63n(4*gib)); err == nil {
					granted = true
				}
			case op < 9: // reap a subtree
				id := pick()
				marked := l.BeginReap(id, api.CodeKilled)
				for _, m := range marked {
					if err := l.Finish(m.ID, api.Termination{Code: m.Code}); err != nil {
						t.Fatalf("seed %d step %d: %v", seed, step, err)
					}
				}
				gone := map[api.LeaseID]bool{}
				for _, m := range marked {
					gone[m.ID] = true
				}
				kept := live[:0]
				for _, x := range live {
					if !gone[x] {
						kept = append(kept, x)
					}
				}
				live = kept
			default: // external load moves the capacity
				capacity = rng.Int63n(64*gib) + gib
				l.SetEffectiveCapacity(capacity)
			}
			_, after, _ := l.Totals()
			if granted && after > before && after > capacity {
				t.Fatalf("seed %d step %d: oversold: allocated %d > capacity %d", seed, step, after, capacity)
			}
			if err := l.Check(); err != nil {
				t.Fatalf("seed %d step %d: %v", seed, step, err)
			}
		}
		for _, id := range l.Leads() {
			for _, m := range l.BeginReap(id, api.CodeKilled) {
				if err := l.Finish(m.ID, api.Termination{}); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, alloc, _ := l.Totals(); alloc != 0 {
			t.Fatalf("seed %d: leaked reservation of %d bytes", seed, alloc)
		}
	}
}
