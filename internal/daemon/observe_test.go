package daemon

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/internal/audit"
	"github.com/zydo/subactl/internal/backend"
	"github.com/zydo/subactl/internal/capacity"
	"github.com/zydo/subactl/internal/telemetry"
)

// world is a manager on a fake backend with a simulated host: 32GiB in
// total, an 8GiB reserve, and an external load the test sets. Available
// memory is what the external load and the agents leave.
type world struct {
	t        *testing.T
	m        *Manager
	f        *backend.Fake
	external atomic.Int64
	events   <-chan api.Event
}

func newWorld(t *testing.T, mutate func(*Config)) *world {
	t.Helper()
	w := &world{t: t, f: backend.NewFake()}
	host := capacity.ReaderFunc(func() (capacity.Host, error) {
		var used int64
		refs, _ := w.f.List(ctx)
		for _, r := range refs { // every acgroup's own usage, once
			used += w.f.OwnUsage(r.Lease)
		}
		return capacity.Host{Total: 32 * gib, Available: 32*gib - w.external.Load() - used}, nil
	})
	cfg := Config{Backend: w.f, Host: host, Reserve: 8 * gib}
	if mutate != nil {
		mutate(&cfg)
	}
	mctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	m, err := NewManager(mctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	w.m = m
	events, stop := m.Events(Caller{Admin: true})
	t.Cleanup(stop)
	w.events = events
	return w
}

// lease acquires and starts a lease whose own processes use mem bytes.
func (w *world) lease(c Caller, req api.AcquireRequest, use int64) api.LeaseID {
	w.t.Helper()
	ls, err := w.m.Acquire(ctx, c, req)
	if err != nil {
		w.t.Fatalf("acquire: %v", err)
	}
	if _, err := w.m.Spawn(ctx, c, ls.ID, api.SpawnRequest{Argv: []string{"agent"}}); err != nil {
		w.t.Fatal(err)
	}
	w.f.SetUsage(ls.ID, use)
	return ls.ID
}

func mem(n int64) api.AcquireRequest {
	return api.AcquireRequest{Kind: "worker", Envelope: api.Envelope{Memory: n}}
}

// next returns the next event of the given type, skipping others, or
// fails if none arrives within d of (fake) time.
func (w *world) next(typ string, d time.Duration) api.Event {
	w.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case e := <-w.events:
			if e.Type == typ {
				return e
			}
		case <-deadline:
			w.t.Fatalf("no %s event within %v", typ, d)
		}
	}
}

// none fails if an event of the given type arrives within d.
func (w *world) none(typ string, d time.Duration) {
	w.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case e := <-w.events:
			if e.Type == typ {
				w.t.Fatalf("unexpected %s event: %+v", typ, e)
			}
		case <-deadline:
			return
		}
	}
}

func (w *world) state(id api.LeaseID) api.State {
	ls, err := w.m.Get(ctx, Caller{Admin: true}, id)
	if err != nil {
		w.t.Fatal(err)
	}
	return ls.State
}

func (w *world) end() {
	w.m.Shutdown()
	synctest.Wait()
}

// F5: the effective capacity follows external load and nothing else.
func TestEffectiveCapacityTracksExternalLoadOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t, nil)
		defer w.end()
		w.external.Store(4 * gib)
		time.Sleep(2 * time.Second)
		if h := w.m.Host(); h.EffectiveCapacity != 20*gib || h.External != 4*gib || h.MemTotal != 32*gib {
			t.Fatalf("host %+v", h)
		}
		a := w.lease(operator, mem(12*gib), 0)
		for _, use := range []int64{gib, 6 * gib, 11 * gib} {
			w.f.SetUsage(a, use)
			time.Sleep(2 * time.Second)
			if h := w.m.Host(); h.EffectiveCapacity != 20*gib || h.External != 4*gib {
				t.Fatalf("with the agent using %d: capacity %d, external %d", use, h.EffectiveCapacity, h.External)
			}
		}
		// 12 of 20 are allocated: 8 are left, whatever the agent uses.
		_, err := w.m.Acquire(ctx, operator, mem(9*gib))
		wantStatus(t, err, http.StatusConflict, api.ErrRejected, api.ReasonMemBudget)
		if _, err := w.m.Acquire(ctx, operator, mem(8*gib)); err != nil {
			t.Fatal(err)
		}
	})
}

// F7: NEAR_LIMIT is pushed within two seconds of usage reaching 90% of the
// hard limit, once, and again only after usage has fallen back.
func TestNearLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t, nil)
		defer w.end()
		id := w.lease(operator, mem(10*gib), 8*gib+900<<20) // 89%
		w.none(telemetry.EventNearLimit, 5*time.Second)

		began := time.Now()
		w.f.SetUsage(id, 9*gib)
		e := w.next(telemetry.EventNearLimit, 2*time.Second)
		if e.Lease != id || e.Data["limit"] != 10*gib || e.Data["usage"] != 9*gib || time.Since(began) > 2*time.Second {
			t.Fatalf("after %v: %+v", time.Since(began), e)
		}
		w.none(telemetry.EventNearLimit, 30*time.Second) // hovering does not repeat it
		w.f.SetUsage(id, 8*gib+800<<20)                  // 88%: not far enough back to re-arm
		w.none(telemetry.EventNearLimit, 5*time.Second)
		w.f.SetUsage(id, 9*gib+500<<20)
		w.none(telemetry.EventNearLimit, 5*time.Second)
		w.f.SetUsage(id, 8*gib) // 80%: re-armed
		time.Sleep(2 * time.Second)
		w.f.SetUsage(id, 9*gib+500<<20)
		w.next(telemetry.EventNearLimit, 2*time.Second)
		if w.state(id) != api.StateRunning {
			t.Fatal("a warning must not terminate anything")
		}
	})
}

// F18: on_near_limit.grow_to takes effect within two seconds when the
// parent has room, does nothing when it has not, and nothing happens for a
// lease that declared no rule.
func TestGrowOnNearLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t, nil)
		defer w.end()
		lead := w.lease(operator, mem(12*gib), 0)
		grow := api.AcquireRequest{Envelope: api.Envelope{Memory: 4 * gib}, OnNearLimit: &api.OnNearLimit{GrowTo: 6 * gib}}
		roomy := w.lease(node(lead), grow, gib)
		plain := w.lease(node(lead), mem(4*gib), gib)

		began := time.Now()
		w.f.SetUsage(roomy, 3*gib+700<<20)
		w.next(telemetry.EventNearLimit, 2*time.Second)
		r := w.next(telemetry.EventResized, 2*time.Second)
		if r.Lease != roomy || r.Data["by"] != "on_near_limit" || r.Data["memory"] != 6*gib || time.Since(began) > 2*time.Second {
			t.Fatalf("after %v: %+v", time.Since(began), r)
		}
		if w.f.Limits(roomy).Memory != 6*gib {
			t.Fatalf("backend limit %d", w.f.Limits(roomy).Memory)
		}
		// Against the new limit the warning is armed again; the parent now
		// has 12 - 6 - 4 = 2 left, so growing a second lease to 8 is refused.
		tight := w.lease(node(lead), api.AcquireRequest{Envelope: api.Envelope{Memory: 2 * gib}, OnNearLimit: &api.OnNearLimit{GrowTo: 8 * gib}}, gib)
		w.f.SetUsage(tight, 2*gib-100<<20)
		w.next(telemetry.EventNearLimit, 2*time.Second)
		w.none(telemetry.EventResized, 5*time.Second)
		if w.f.Limits(tight).Memory != 2*gib {
			t.Fatal("grew beyond the parent's room")
		}
		// No rule declared: a warning and nothing else.
		w.f.SetUsage(plain, 3*gib+900<<20)
		w.next(telemetry.EventNearLimit, 2*time.Second)
		w.none(telemetry.EventResized, 5*time.Second)
		if w.f.Limits(plain).Memory != 4*gib {
			t.Fatal("a lease without a rule was resized")
		}
	})
}

// F8, first half: external load pushes the effective capacity below what
// is allocated. CAPACITY_SHRUNK comes within a second, new leases and
// raises are refused, and nothing is terminated.
func TestCapacityShrunkRejectsOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t, nil)
		defer w.end()
		a := w.lease(operator, mem(14*gib), gib)
		b := w.lease(operator, mem(8*gib), gib)
		w.none(telemetry.EventCapacityShrunk, 3*time.Second)

		began := time.Now()
		w.external.Store(6 * gib) // capacity 18 < 22 allocated; available 24, well above the reserve
		e := w.next(telemetry.EventCapacityShrunk, time.Second)
		if e.Data["reason"] != "allocated_exceeds_capacity" || e.Data["allocated"] != 22*gib || time.Since(began) > time.Second {
			t.Fatalf("after %v: %+v", time.Since(began), e)
		}
		_, err := w.m.Acquire(ctx, operator, mem(gib))
		wantStatus(t, err, http.StatusConflict, api.ErrRejected, api.ReasonCapacityShrunk)
		up := 9 * gib
		_, err = w.m.Resize(ctx, operator, b, api.EnvelopePatch{Memory: &up})
		wantStatus(t, err, http.StatusConflict, api.ErrRejected, api.ReasonCapacityShrunk)
		w.none(telemetry.EventTerminated, 30*time.Second)
		if w.state(a) != api.StateRunning || w.state(b) != api.StateRunning || !w.m.Host().CapacityShrunk {
			t.Fatal("a lease was touched, or the host does not report the shrink")
		}
		// It recovers by itself, and says so again the next time.
		w.external.Store(0)
		time.Sleep(2 * time.Second)
		if _, err := w.m.Acquire(ctx, operator, mem(gib)); err != nil {
			t.Fatal(err)
		}
		w.external.Store(8 * gib)
		w.next(telemetry.EventCapacityShrunk, 2*time.Second)
	})
}

// F8, second half, with F18 and F29: memory falls below the reserve line.
// After the warning and the window, leases are terminated one at a time in
// the declared order, with 503, and not one more than needed.
func TestSafetyNet(t *testing.T) {
	type tree struct{ a, a1, a2, b, b1 api.LeaseID }
	build := func(w *world, a1Order *int) tree {
		var tr tree
		tr.a = w.lease(operator, api.AcquireRequest{Kind: "lead", Envelope: api.Envelope{Memory: 10 * gib}}, gib)
		tr.a1 = w.lease(node(tr.a), api.AcquireRequest{Envelope: api.Envelope{Memory: 3 * gib}, KillOrder: a1Order}, 2*gib)
		tr.a2 = w.lease(node(tr.a), mem(4*gib), 3*gib)
		tr.b = w.lease(Caller{UID: 502}, mem(6*gib), 2*gib)
		// B's planner has a hint too; it must not reach into A's tree.
		one := 1
		tr.b1 = w.lease(node(tr.b), api.AcquireRequest{Envelope: api.Envelope{Memory: 2 * gib}, KillOrder: &one}, gib)
		return tr
	}
	// The agents use 9GiB: A's tree 6, B's tree 3.
	t.Run("one is enough", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := newWorld(t, nil)
			defer w.end()
			tr := build(w, nil)
			time.Sleep(3 * time.Second)
			w.none(telemetry.EventTerminated, 5*time.Second)

			began := time.Now()
			w.external.Store(17 * gib) // available 32 - 17 - 9 = 6, below the 8GiB line
			warn := w.next(telemetry.EventCapacityShrunk, 10*time.Second)
			for warn.Data["reason"] != "below_reserve" {
				warn = w.next(telemetry.EventCapacityShrunk, 10*time.Second)
			}
			if d := time.Since(began); d < 2*time.Second || d > 4*time.Second {
				t.Fatalf("the warning came after %v; it needs three readings in a row", d)
			}
			// H4: the planner hears before anything is terminated.
			term := w.next(telemetry.EventTerminated, 10*time.Second)
			if d := time.Since(began); d < 7*time.Second || d > 10*time.Second {
				t.Fatalf("the first termination came %v after the pressure began; want the 5s window after the warning", d)
			}
			// The largest lead is A; with no hint, its largest leaf goes.
			if term.Lease != tr.a2 || term.Data["code"] != float64(api.CodeSafetyNet) {
				t.Fatalf("terminated %+v, want a2 with 503", term)
			}
			// Available is now 9, above the line: nobody else goes.
			w.none(telemetry.EventTerminated, 60*time.Second)
			for _, id := range []api.LeaseID{tr.a, tr.a1, tr.b, tr.b1} {
				if w.state(id) != api.StateRunning {
					t.Fatalf("%s was terminated too", id)
				}
			}
			if n := w.m.obs.metrics.SafetyNetCount(); n != 1 {
				t.Fatalf("safety net count %d", n)
			}
		})
	})
	t.Run("declared order, two needed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := newWorld(t, nil)
			defer w.end()
			two := 2
			tr := build(w, &two)
			time.Sleep(3 * time.Second)
			w.external.Store(19 * gib) // available 4: two leases have to go
			first := w.next(telemetry.EventTerminated, 20*time.Second)
			at := time.Now()
			// A's planner declared a1 first, though a2 is larger. B's hint
			// of 1 is lower still, but it is another tree's.
			if first.Lease != tr.a1 {
				t.Fatalf("first %+v, want a1 (kill_order 2 inside the largest lead)", first)
			}
			// Available is 6, still below. The next one goes only after
			// three fresh readings, not at once.
			second := w.next(telemetry.EventTerminated, 20*time.Second)
			if d := time.Since(at); second.Lease != tr.a2 || d < 2*time.Second {
				t.Fatalf("second %+v after %v", second, d)
			}
			// Available is 9: it stops.
			w.none(telemetry.EventTerminated, 60*time.Second)
			if w.state(tr.a) != api.StateRunning || w.state(tr.b) != api.StateRunning || w.state(tr.b1) != api.StateRunning {
				t.Fatal("more was terminated than needed")
			}
		})
	})
	t.Run("released in the window", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := newWorld(t, nil)
			defer w.end()
			tr := build(w, nil)
			time.Sleep(3 * time.Second)
			w.external.Store(17 * gib)
			warn := w.next(telemetry.EventCapacityShrunk, 10*time.Second)
			for warn.Data["reason"] != "below_reserve" {
				warn = w.next(telemetry.EventCapacityShrunk, 10*time.Second)
			}
			// A code planner reacts inside the window and releases a lease
			// of its own choosing.
			if _, err := w.m.Kill(ctx, node(tr.a), tr.a1); err != nil {
				t.Fatal(err)
			}
			w.next(telemetry.EventTerminated, time.Second)
			w.none(telemetry.EventTerminated, 60*time.Second)
			if n := w.m.obs.metrics.SafetyNetCount(); n != 0 {
				t.Fatalf("the safety net fired %d times although the planner released in time", n)
			}
		})
	})
}

// Events reach only those who may see them.
func TestEventVisibility(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t, nil)
		defer w.end()
		lead := w.lease(operator, mem(8*gib), 0)
		a := w.lease(node(lead), mem(2*gib), 0)
		b := w.lease(node(lead), mem(2*gib), 0)
		other := w.lease(Caller{UID: 502}, mem(2*gib), 0)
		seen := func(c Caller) map[api.LeaseID]bool {
			events, stop := w.m.Events(c)
			defer stop()
			for _, id := range []api.LeaseID{a, b, other} {
				w.f.SetUsage(id, 2*gib) // NEAR_LIMIT for each
			}
			time.Sleep(3 * time.Second)
			out := map[api.LeaseID]bool{}
			for len(events) > 0 {
				out[(<-events).Lease] = true
			}
			for _, id := range []api.LeaseID{a, b, other} {
				w.f.SetUsage(id, 0)
			}
			time.Sleep(3 * time.Second)
			return out
		}
		if got := seen(node(a)); !got[a] || got[b] || got[other] {
			t.Errorf("node a saw %v", got)
		}
		if got := seen(node(lead)); !got[a] || !got[b] || got[other] {
			t.Errorf("the lead saw %v", got)
		}
		if got := seen(operator); !got[a] || !got[b] || got[other] {
			t.Errorf("the operator saw %v", got)
		}
		if got := seen(Caller{UID: 502}); got[a] || !got[other] {
			t.Errorf("the other operator saw %v", got)
		}
	})
}

// Audit, suggestions, trace and metrics, across a daemon restart.
func TestAuditSuggestTraceMetrics(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit")
	log := audit.Open(dir)
	var leadID, lastID api.LeaseID
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t, func(c *Config) { c.Audit, c.AuditDir = log, dir })
		leadID = w.lease(operator, api.AcquireRequest{Kind: "lead", Envelope: api.Envelope{Memory: 16 * gib}}, 0)
		// Twenty workers with peaks of 100MiB to 2000MiB.
		for i := 1; i <= 20; i++ {
			id := w.lease(node(leadID), mem(4*gib), int64(i)*100<<20)
			time.Sleep(6 * time.Second) // long enough for a 5s audit sample
			w.f.Exit(id, backend.ExitStatus{})
			w.m.waitReaped(ctx, id)
			lastID = id
		}
		a, err := w.m.Available(node(leadID), api.Self, "renderer")
		if err != nil {
			t.Fatal(err)
		}
		// p95 of 100..2000MiB is 1900MiB; times 1.2.
		if s := a.Suggest["worker"]; s.Default || s.Samples != 20 || s.Memory != 1900<<20*12/10 {
			t.Fatalf("worker suggestion %+v", s)
		}
		if s := a.Suggest["renderer"]; !s.Default || s.Memory != 2*gib {
			t.Fatalf("renderer suggestion %+v", s)
		}
		// The trace of a reaped lease, from memory: every second.
		tr, err := w.m.Trace(node(leadID), lastID)
		if err != nil || len(tr.Samples) < 5 || tr.Termination == nil || tr.Termination.Code != api.CodeCompleted {
			t.Fatalf("trace %+v, %v", tr, err)
		}
		types := ""
		for _, e := range tr.Events {
			types += e.Type + " "
		}
		if types != "STATE STATE TERMINATED " {
			t.Fatalf("events %q", types)
		}
		if _, err := w.m.Trace(Caller{UID: 502}, lastID); err == nil {
			t.Fatal("another user read the trace")
		}
		text := w.m.MetricsText()
		for _, want := range []string{`subactl_terminations_total{code="200"} 20`, `subactl_live_leases{depth="0"} 1`, `subactl_lease_peak_bytes_count{kind="worker"} 20`, "subactl_effective_capacity_bytes 25769803776"} {
			if !strings.Contains(text, want) {
				t.Errorf("metrics lack %q", want)
			}
		}
		w.end()
	})
	log.Close()

	// A new daemon: nothing in memory, the history is on disk.
	sug := audit.NewSuggester(2*gib, api.BasisWorkingSet)
	if err := sug.Load(dir); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(ctx, Config{Backend: backend.NewFake(), Capacity: 32 * gib, AuditDir: dir, Suggest: sug})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := m.Trace(operator, lastID)
	if err != nil || tr.Termination == nil || tr.Termination.Peak != 2000<<20 || len(tr.Samples) == 0 || len(tr.Events) != 3 {
		t.Fatalf("trace from the audit log: %+v, %v", tr, err)
	}
	if _, err := m.Trace(Caller{UID: 502}, lastID); err == nil {
		t.Fatal("another user read the trace from the audit log")
	}
	if _, err := m.Trace(operator, "ls_never"); err == nil {
		t.Fatal("a trace for a lease that never existed")
	}
	lead, _ := m.Acquire(ctx, operator, mem(gib))
	a, _ := m.Available(operator, lead.ID)
	if s := a.Suggest["worker"]; s.Samples != 20 || s.Default {
		t.Fatalf("history was not reloaded: %+v", s)
	}
	_ = leadID
}

// F32: with the audit directory unwritable, warnings and kills are as
// prompt as ever, and the losses are counted.
func TestObservabilityFailureDoesNotSlowEnforcement(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(parent, 0o700)
	log := audit.Open(filepath.Join(parent, "audit"))
	defer log.Close()
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t, func(c *Config) { c.Audit = log })
		defer w.end()
		id := w.lease(operator, mem(10*gib), gib)
		w.f.AddProcs(id, 3)
		began := time.Now()
		w.f.SetUsage(id, 9*gib+500<<20)
		w.next(telemetry.EventNearLimit, 2*time.Second)
		if d := time.Since(began); d > 2*time.Second {
			t.Fatalf("NEAR_LIMIT took %v", d)
		}
		began = time.Now()
		if _, err := w.m.Kill(ctx, operator, id); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(began); d != 0 {
			t.Fatalf("kill_tree took %v of scheduled time", d)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for log.Dropped() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if log.Dropped() == 0 {
		t.Fatal("nothing was counted as dropped")
	}
}
