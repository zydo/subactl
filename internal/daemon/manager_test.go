package daemon

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/internal/backend"
)

const gib = int64(1) << 30

var (
	ctx      = context.Background()
	operator = Caller{UID: 501}
)

func node(id api.LeaseID) Caller { return Caller{Lease: id, UID: 501} }

func newManager(t *testing.T) (*Manager, *backend.Fake) {
	t.Helper()
	f := backend.NewFake()
	// The manager's background loops end with the test.
	mctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	m, err := NewManager(mctx, Config{Backend: f, Capacity: 32 * gib})
	if err != nil {
		t.Fatal(err)
	}
	return m, f
}

// start acquires a lease and spawns its main process.
func start(t *testing.T, m *Manager, c Caller, parent api.LeaseID, mem int64) api.LeaseID {
	t.Helper()
	ls, err := m.Acquire(ctx, c, api.AcquireRequest{Parent: parent, Kind: "worker", Envelope: api.Envelope{Memory: mem}})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := m.Spawn(ctx, c, ls.ID, api.SpawnRequest{Argv: []string{"agent"}}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	return ls.ID
}

func wait(t *testing.T, m *Manager, id api.LeaseID) api.Termination {
	t.Helper()
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	term, ok := m.waitReaped(wctx, id)
	if !ok {
		t.Fatalf("lease %s was not reaped", id)
	}
	return term
}

func wantStatus(t *testing.T, err error, status int, code, reason string) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Status != status || e.API.Code != code || e.API.Reason != reason {
		t.Fatalf("got %v, want HTTP %d %s %s", err, status, code, reason)
	}
	return e
}

func TestLifecycleCompleted(t *testing.T) {
	m, f := newManager(t)
	lead := start(t, m, operator, "", 8*gib)
	child := start(t, m, node(lead), "", 2*gib) // parent defaults to the caller
	if ls, _ := m.Get(ctx, operator, child); ls.Parent != lead || ls.Depth != 1 || ls.State != api.StateRunning || ls.Usage == nil {
		t.Fatalf("child %+v", ls)
	}
	f.Exit(child, backend.ExitStatus{})
	if term := wait(t, m, child); term.Code != api.CodeCompleted || term.Name != "COMPLETED" {
		t.Fatalf("termination %+v", term)
	}
	// The reaped child stays queryable by its parent, with its record.
	ls, err := m.Get(ctx, node(lead), child)
	if err != nil || ls.State != api.StateReaped || ls.Termination == nil || ls.Usage != nil {
		t.Fatalf("got %+v, %v", ls, err)
	}
	if f.Exists(child) {
		t.Fatal("acgroup of a reaped lease still exists")
	}
	if a, _ := m.Available(node(lead), api.Self); a.MemoryFree != 8*gib || a.LiveChildren != 0 {
		t.Fatalf("reservation not released: %+v", a)
	}
}

// F2: an external SIGKILL of the main process gives 520 and no descendant
// survives; a clean exit that leaves descendants gives 200 and lists them.
func TestMainExitReapsDescendants(t *testing.T) {
	m, f := newManager(t)
	a := start(t, m, operator, "", 4*gib)
	f.AddProcs(a, 11) // e.g. Chrome renderers
	f.Exit(a, backend.ExitStatus{Signal: int(syscall.SIGKILL)})
	term := wait(t, m, a)
	if term.Code != api.CodeExternalAbort || term.Evidence["signal"] != int(syscall.SIGKILL) || term.Evidence["reaped_pids"] != 11 {
		t.Fatalf("termination %+v", term)
	}

	b := start(t, m, operator, "", 4*gib)
	f.AddProcs(b, 3)
	f.Exit(b, backend.ExitStatus{})
	if term := wait(t, m, b); term.Code != api.CodeCompleted || term.Evidence["reaped_pids"] != 3 {
		t.Fatalf("termination %+v", term)
	}

	c := start(t, m, operator, "", 4*gib)
	f.Exit(c, backend.ExitStatus{Code: 3}) // a crash: non-zero exit, no signal
	if term := wait(t, m, c); term.Code != api.CodeExternalAbort || term.Evidence["exit_code"] != 3 {
		t.Fatalf("termination %+v", term)
	}
	if refs, _ := f.List(ctx); len(refs) != 0 {
		t.Fatalf("acgroups left: %v", refs)
	}
}

// F1/F15 at the ledger level: when a node ends, its whole subtree is reaped
// with 502 and nothing is left allocated.
func TestCascade(t *testing.T) {
	m, f := newManager(t)
	lead := start(t, m, operator, "", 16*gib)
	w1 := start(t, m, node(lead), "", 4*gib)
	w2 := start(t, m, node(lead), "", 4*gib)
	ww := start(t, m, node(w1), "", gib)
	f.AddProcs(ww, 5)

	f.Exit(lead, backend.ExitStatus{Signal: int(syscall.SIGKILL)})
	if term := wait(t, m, lead); term.Code != api.CodeExternalAbort || term.Evidence["reaped_pids"] != 5+3 {
		t.Fatalf("lead %+v", term)
	}
	for _, id := range []api.LeaseID{w1, w2, ww} {
		if term := wait(t, m, id); term.Code != api.CodeParentDied {
			t.Fatalf("%s: %+v", id, term)
		}
	}
	if refs, _ := f.List(ctx); len(refs) != 0 {
		t.Fatalf("acgroups left: %v", refs)
	}
	if h := m.Host(); h.Allocated != 0 {
		t.Fatalf("allocated %d after the tree was reaped", h.Allocated)
	}
}

func TestKill(t *testing.T) {
	m, _ := newManager(t)
	lead := start(t, m, operator, "", 8*gib)
	w := start(t, m, node(lead), "", gib)
	term, err := m.Kill(ctx, node(lead), w)
	if err != nil || term.Code != api.CodeKilled || term.Evidence["step"] != "kill" {
		t.Fatalf("got %+v, %v", term, err)
	}
	// Killing again is harmless and returns the same single record.
	again, err := m.Kill(ctx, node(lead), w)
	if err != nil || !again.At.Equal(term.At) {
		t.Fatalf("got %+v, %v", again, err)
	}
	if _, err := m.Kill(ctx, operator, lead); err != nil {
		t.Fatal(err)
	}
}

// F17: the terminate ladder. Run under synctest, so 30s and 10s are exact
// and take no real time.
func TestTerminateLadder(t *testing.T) {
	grace := func(s float64) *float64 { return &s }
	cases := []struct {
		name     string
		prepare  func(f *backend.Fake, id api.LeaseID)
		during   func(f *backend.Fake, id api.LeaseID) // runs 5s into the grace period
		req      api.TerminateRequest
		wantCode int
		wantStep string
		wantTook time.Duration
	}{
		{name: "exits during grace", req: api.TerminateRequest{Code: api.CodeReleased},
			during:   func(f *backend.Fake, id api.LeaseID) { f.Exit(id, backend.ExitStatus{}) },
			wantCode: api.CodeReleased, wantStep: "grace", wantTook: 5 * time.Second},
		{name: "exits on SIGTERM", req: api.TerminateRequest{},
			wantCode: api.CodeKilled, wantStep: "sigterm", wantTook: 30 * time.Second},
		{name: "ignores SIGTERM", req: api.TerminateRequest{GraceSeconds: grace(20), Code: api.CodeReleased},
			prepare:  func(f *backend.Fake, id api.LeaseID) { f.IgnoreTERM(id) },
			wantCode: api.CodeReleased, wantStep: "kill_tree", wantTook: 30 * time.Second},
		{name: "zero grace", req: api.TerminateRequest{GraceSeconds: grace(0)},
			prepare:  func(f *backend.Fake, id api.LeaseID) { f.IgnoreTERM(id) },
			wantCode: api.CodeKilled, wantStep: "kill_tree", wantTook: 10 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, f := newManager(t)
				id := start(t, m, operator, "", gib)
				kid := start(t, m, node(id), "", gib/2)
				if tc.prepare != nil {
					tc.prepare(f, id)
				}
				if tc.during != nil {
					go func() {
						time.Sleep(5 * time.Second)
						tc.during(f, id)
					}()
				}
				began := time.Now()
				term, err := m.Terminate(ctx, operator, id, tc.req)
				if err != nil {
					t.Fatal(err)
				}
				if term.Code != tc.wantCode || term.Evidence["step"] != tc.wantStep {
					t.Fatalf("termination %+v", term)
				}
				if took := time.Since(began); took != tc.wantTook {
					t.Fatalf("took %v, want %v", took, tc.wantTook)
				}
				if kt, ok := m.waitReaped(ctx, kid); !ok || kt.Code != api.CodeParentDied {
					t.Fatalf("child %+v", kt)
				}
				synctest.Wait()
			})
		})
	}
}

func TestTerminateNeverSpawnedAndValidation(t *testing.T) {
	m, _ := newManager(t)
	ls, err := m.Acquire(ctx, operator, api.AcquireRequest{Envelope: api.Envelope{Memory: gib}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Terminate(ctx, operator, ls.ID, api.TerminateRequest{Code: 200})
	if e := wantStatus(t, err, http.StatusBadRequest, api.ErrInvalidArgument, ""); e.API.Field != "code" || e.API.Example == "" {
		t.Fatalf("error %+v", e.API)
	}
	term, err := m.Terminate(ctx, operator, ls.ID, api.TerminateRequest{Code: api.CodeReleased})
	if err != nil || term.Code != api.CodeReleased || term.Evidence["step"] != "not_started" {
		t.Fatalf("got %+v, %v", term, err)
	}
}

func TestReapIncomplete(t *testing.T) {
	m, f := newManager(t)
	id := start(t, m, operator, "", gib)
	f.AddStuckProcs(id, 2)
	term, err := m.Kill(ctx, operator, id)
	if err != nil || term.Code != api.CodeReapIncomplete || term.Evidence["remaining_pids"] != 2 || term.Evidence["intended_code"] != api.CodeKilled {
		t.Fatalf("got %+v, %v", term, err)
	}
}

// F12 at the manager level: acgroups left by a previous daemon are killed
// and destroyed at startup, children before parents, and never adopted.
func TestFailClosedStartup(t *testing.T) {
	f := backend.NewFake()
	old, err := NewManager(ctx, Config{Backend: f, Capacity: 32 * gib})
	if err != nil {
		t.Fatal(err)
	}
	lead := start(t, old, operator, "", 8*gib)
	kid := start(t, old, node(lead), "", gib)
	f.AddProcs(kid, 4)
	// The old daemon "crashes": its manager is simply abandoned.

	m, err := NewManager(ctx, Config{Backend: f, Capacity: 32 * gib})
	if err != nil {
		t.Fatal(err)
	}
	if refs, _ := f.List(ctx); len(refs) != 0 {
		t.Fatalf("acgroups survived a restart: %v", refs)
	}
	if h := m.Host(); h.Allocated != 0 {
		t.Fatalf("new daemon adopted %d bytes", h.Allocated)
	}
	if _, err := m.Get(ctx, operator, lead); err == nil {
		t.Fatal("new daemon knows a lease from before the restart")
	}
}

// F16: a node acts only inside its own subtree, and never resizes itself.
func TestSubtreePermissions(t *testing.T) {
	m, f := newManager(t)
	lead := start(t, m, operator, "", 16*gib)
	a := start(t, m, node(lead), "", 4*gib)
	b := start(t, m, node(lead), "", 4*gib)
	other := start(t, m, Caller{UID: 502}, "", 4*gib)
	two := 2 * gib
	patch := api.EnvelopePatch{Memory: &two}

	forbidden := func(err error, code string) {
		t.Helper()
		wantStatus(t, err, http.StatusForbidden, code, "")
	}
	_, err := m.Kill(ctx, node(a), b) // sibling
	forbidden(err, api.ErrOutOfSubtree)
	_, err = m.Terminate(ctx, node(a), lead, api.TerminateRequest{}) // ancestor
	forbidden(err, api.ErrOutOfSubtree)
	_, err = m.Get(ctx, node(a), other) // another lead's tree
	forbidden(err, api.ErrOutOfSubtree)
	_, err = m.Resize(ctx, node(a), a, patch) // itself
	forbidden(err, api.ErrSelfResize)
	_, err = m.Resize(ctx, node(a), api.Self, patch)
	forbidden(err, api.ErrSelfResize)
	_, err = m.Acquire(ctx, node(a), api.AcquireRequest{Parent: b, Envelope: api.Envelope{Memory: gib}})
	forbidden(err, api.ErrOutOfSubtree)
	_, err = m.Kill(ctx, operator, other) // another user's lead
	forbidden(err, api.ErrOutOfSubtree)
	_, err = m.Spawn(ctx, Caller{UID: 502}, a, api.SpawnRequest{Argv: []string{"x"}})
	forbidden(err, api.ErrOutOfSubtree)

	// None of that had any effect.
	for _, id := range []api.LeaseID{lead, a, b, other} {
		if ls, _ := m.Get(ctx, Caller{Admin: true}, id); ls.State != api.StateRunning || ls.Envelope.Memory == two {
			t.Fatalf("%s was affected: %+v", id, ls)
		}
	}
	if f.Limits(a).Memory != 4*gib {
		t.Fatal("limits changed")
	}

	// What each caller sees.
	count := func(c Caller) int {
		ls, err := m.List(ctx, c, "")
		if err != nil {
			t.Fatal(err)
		}
		return len(ls)
	}
	if n := count(node(a)); n != 1 {
		t.Errorf("node sees %d leases, want its own subtree of 1", n)
	}
	if n := count(operator); n != 3 {
		t.Errorf("operator sees %d leases, want its own tree of 3", n)
	}
	if n := count(Caller{Admin: true}); n != 4 {
		t.Errorf("admin sees %d leases, want all 4", n)
	}
	if _, err := m.List(ctx, operator, other); err == nil {
		t.Error("operator listed another user's lead")
	}
	// The parent may do all of it.
	if _, err := m.Resize(ctx, node(lead), a, patch); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Kill(ctx, node(lead), b); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionRejections(t *testing.T) {
	m, f := newManager(t)
	lead := start(t, m, operator, "", 20*gib)

	_, err := m.Acquire(ctx, operator, api.AcquireRequest{Envelope: api.Envelope{Memory: 13 * gib}})
	wantStatus(t, err, http.StatusConflict, api.ErrRejected, api.ReasonMemBudget)
	_, err = m.Acquire(ctx, node(lead), api.AcquireRequest{Envelope: api.Envelope{Memory: 21 * gib}})
	wantStatus(t, err, http.StatusConflict, api.ErrRejected, api.ReasonParentBudget)

	// F13: a backend that cannot reach the required level says so.
	caps := f.Capabilities()
	caps.Memory = api.LevelMonitorOnly
	f.SetCapabilities(caps)
	req := api.AcquireRequest{Envelope: api.Envelope{Memory: gib}, Require: &api.Require{Memory: api.LevelHard}}
	_, err = m.Acquire(ctx, node(lead), req)
	wantStatus(t, err, http.StatusConflict, api.ErrRejected, api.ReasonEnforcementUnavailable)
	req.Require.Memory = api.LevelMonitorOnly
	if _, err := m.Acquire(ctx, node(lead), req); err != nil {
		t.Fatal(err)
	}

	// A backend failure releases the reservation.
	f.CreateErr = errors.New("boom")
	_, err = m.Acquire(ctx, operator, api.AcquireRequest{Envelope: api.Envelope{Memory: 12 * gib}})
	wantStatus(t, err, http.StatusInternalServerError, api.ErrInternal, "")
	if _, err := m.Acquire(ctx, operator, api.AcquireRequest{Envelope: api.Envelope{Memory: 12 * gib}}); err != nil {
		t.Fatalf("reservation leaked after a failed create: %v", err)
	}
	// With nothing left, a default lead envelope is refused, not zero-sized.
	_, err = m.Acquire(ctx, operator, api.AcquireRequest{})
	wantStatus(t, err, http.StatusConflict, api.ErrRejected, api.ReasonMemBudget)
}

func TestDefaultLeadEnvelope(t *testing.T) {
	m, _ := newManager(t)
	a, err := m.Acquire(ctx, operator, api.AcquireRequest{})
	if err != nil || a.Envelope.Memory != 16*gib { // 50% of 32GiB
		t.Fatalf("got %+v, %v", a.Envelope, err)
	}
	if _, err := m.Acquire(ctx, operator, api.AcquireRequest{Envelope: api.Envelope{Memory: 10 * gib}}); err != nil {
		t.Fatal(err)
	}
	b, err := m.Acquire(ctx, operator, api.AcquireRequest{})
	if err != nil || b.Envelope.Memory != 6*gib { // what is left
		t.Fatalf("got %+v, %v", b.Envelope, err)
	}
}

// F19: an invalid request is refused as a whole, with the field, what was
// expected and an example, and has no side effect.
func TestValidation(t *testing.T) {
	m, f := newManager(t)
	lead := start(t, m, operator, "", 8*gib)
	uid0, uid7 := 0, 7
	cases := []struct {
		field  string
		caller Caller
		req    api.AcquireRequest
	}{
		{"envelope.memory", node(lead), api.AcquireRequest{}},
		{"envelope.memory", operator, api.AcquireRequest{Envelope: api.Envelope{Memory: -1}}},
		{"envelope.memory_self", operator, api.AcquireRequest{Envelope: api.Envelope{Memory: gib, MemorySelf: 2 * gib}}},
		{"envelope.cpu", operator, api.AcquireRequest{Envelope: api.Envelope{Memory: gib, CPU: -1}}},
		{"envelope.wall", operator, api.AcquireRequest{Envelope: api.Envelope{Memory: gib, Wall: -1}}},
		{"kind", operator, api.AcquireRequest{Kind: "Bad Kind", Envelope: api.Envelope{Memory: gib}}},
		{"require.memory", operator, api.AcquireRequest{Envelope: api.Envelope{Memory: gib}, Require: &api.Require{Memory: "strict"}}},
		{"on_near_limit.grow_to", operator, api.AcquireRequest{Envelope: api.Envelope{Memory: gib}, OnNearLimit: &api.OnNearLimit{GrowTo: gib}}},
		{"uid", node(lead), api.AcquireRequest{UID: &uid7, Envelope: api.Envelope{Memory: gib}}},
		{"tty", node(lead), api.AcquireRequest{TTY: true, Envelope: api.Envelope{Memory: gib}}},
		{"uid", operator, api.AcquireRequest{UID: &uid7, Envelope: api.Envelope{Memory: gib}}},
		// F33: no agent runs as root.
		{"uid", Caller{UID: 0, Admin: true}, api.AcquireRequest{Envelope: api.Envelope{Memory: gib}}},
		{"uid", Caller{UID: 0, Admin: true}, api.AcquireRequest{UID: &uid0, Envelope: api.Envelope{Memory: gib}}},
	}
	for _, tc := range cases {
		_, err := m.Acquire(ctx, tc.caller, tc.req)
		e := wantStatus(t, err, http.StatusBadRequest, api.ErrInvalidArgument, "")
		if e.API.Field != tc.field || e.API.Expected == "" || e.API.Message == "" {
			t.Errorf("want field %s with an expectation, got %+v", tc.field, e.API)
		}
	}
	if refs, _ := f.List(ctx); len(refs) != 1 {
		t.Fatalf("invalid requests created acgroups: %v", refs)
	}
	// Root may start a lead as a named non-root user, who then owns... the
	// operator, not the run-as uid, owns the tree.
	ls, err := m.Acquire(ctx, Caller{UID: 0, Admin: true}, api.AcquireRequest{UID: &uid7, Envelope: api.Envelope{Memory: gib}})
	if err != nil || ls.UID != 7 || ls.OwnerUID != 0 {
		t.Fatalf("got %+v, %v", ls, err)
	}
	if _, err := m.Spawn(ctx, operator, lead, api.SpawnRequest{}); err == nil {
		t.Fatal("empty argv accepted")
	}
	if _, err := m.Spawn(ctx, operator, lead, api.SpawnRequest{Argv: []string{"x"}}); err == nil {
		t.Fatal("second main process accepted")
	}
}

func TestResizeAppliesLimits(t *testing.T) {
	m, f := newManager(t)
	lead := start(t, m, operator, "", 8*gib)
	w := start(t, m, node(lead), "", 2*gib)
	f.SetUsage(w, gib)
	v := func(n int64) *int64 { return &n }

	if _, err := m.Resize(ctx, node(lead), w, api.EnvelopePatch{Memory: v(4 * gib), DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if f.Limits(w).Memory != 2*gib {
		t.Fatal("dry run changed the limit")
	}
	ls, err := m.Resize(ctx, node(lead), w, api.EnvelopePatch{Memory: v(4 * gib), Pids: v(64)})
	if err != nil || ls.Envelope.Memory != 4*gib || f.Limits(w).Memory != 4*gib || f.Limits(w).Pids != 64 {
		t.Fatalf("got %+v, %v; backend %+v", ls.Envelope, err, f.Limits(w))
	}
	_, err = m.Resize(ctx, node(lead), w, api.EnvelopePatch{Memory: v(9 * gib)})
	wantStatus(t, err, http.StatusConflict, api.ErrRejected, api.ReasonParentBudget)
	_, err = m.Resize(ctx, node(lead), w, api.EnvelopePatch{Memory: v(gib)}) // usage is 1GiB
	wantStatus(t, err, http.StatusConflict, api.ErrRejected, api.ReasonBelowUsage)
	_, err = m.Resize(ctx, node(lead), w, api.EnvelopePatch{Memory: v(-5)})
	wantStatus(t, err, http.StatusBadRequest, api.ErrInvalidArgument, "")
	if f.Limits(w).Memory != 4*gib {
		t.Fatal("a refused resize changed the limit")
	}
}

func TestWaitChildren(t *testing.T) {
	m, f := newManager(t)
	lead := start(t, m, operator, "", 8*gib)
	a := start(t, m, node(lead), "", gib)
	b := start(t, m, node(lead), "", gib)
	f.Exit(a, backend.ExitStatus{})
	wait(t, m, a)

	got, err := m.WaitChildren(ctx, node(lead), api.Self, api.WaitChildrenRequest{TimeoutSeconds: 0.05})
	if err != nil || got.Done || len(got.Results) != 1 || got.Results[0].Lease != a {
		t.Fatalf("got %+v, %v", got, err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.Exit(b, backend.ExitStatus{Code: 1})
	}()
	got, err = m.WaitChildren(ctx, node(lead), api.Self, api.WaitChildrenRequest{})
	if err != nil || !got.Done || len(got.Results) != 2 || got.Results[1].Code != api.CodeExternalAbort {
		t.Fatalf("got %+v, %v", got, err)
	}
	// With self, the lease's own record comes first (pre-order).
	go m.Kill(ctx, operator, lead)
	got, err = m.WaitChildren(ctx, operator, lead, api.WaitChildrenRequest{Self: true})
	if err != nil || !got.Done || len(got.Results) != 3 || got.Results[0].Lease != lead {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// F10's core property: however many causes race, a lease ends with exactly
// one termination record.
func TestExactlyOneTerminationUnderRaces(t *testing.T) {
	m, f := newManager(t)
	for i := 0; i < 200; i++ {
		lead := start(t, m, operator, "", 2*gib)
		kid := start(t, m, node(lead), "", gib)
		var wg sync.WaitGroup
		zero := 0.0
		for _, fn := range []func(){
			func() { f.ExitIfAlive(kid, backend.ExitStatus{}) },
			func() { f.ExitIfAlive(lead, backend.ExitStatus{Signal: 9}) },
			func() { m.Kill(ctx, operator, kid) },
			func() { m.Kill(ctx, operator, lead) },
			func() { m.Terminate(ctx, operator, lead, api.TerminateRequest{GraceSeconds: &zero}) },
		} {
			wg.Add(1)
			go func() { defer wg.Done(); fn() }()
		}
		wg.Wait()
		for _, id := range []api.LeaseID{lead, kid} {
			first := wait(t, m, id)
			if first.Name == "" {
				t.Fatalf("%s: no termination code: %+v", id, first)
			}
			if again := wait(t, m, id); !again.At.Equal(first.At) || again.Code != first.Code {
				t.Fatalf("%s: record changed from %+v to %+v", id, first, again)
			}
		}
	}
	if refs, _ := f.List(ctx); len(refs) != 0 {
		t.Fatalf("%d acgroups left", len(refs))
	}
	if h := m.Host(); h.Allocated != 0 {
		t.Fatalf("allocated %d", h.Allocated)
	}
}

// F4 and F14: a lease that hits its own memory limit ends with 413; a
// nested lease that dies because its parent hit the limit ends with 502;
// leases outside are untouched.
func TestOOMAttribution(t *testing.T) {
	m, f := newManager(t)
	lead := start(t, m, operator, "", 16*gib)
	a := start(t, m, node(lead), "", 4*gib)
	aa := start(t, m, node(a), "", gib)
	b := start(t, m, node(lead), "", 4*gib)

	f.OOM(a) // a's limit: the kernel kills a and aa together
	if term := wait(t, m, a); term.Code != api.CodeOOMLeaseLimit || term.Dimension != "memory" || term.Limit != 4*gib {
		t.Fatalf("a: %+v", term)
	}
	if term := wait(t, m, aa); term.Code != api.CodeParentDied {
		t.Fatalf("aa: %+v", term)
	}
	for _, id := range []api.LeaseID{lead, b} {
		if ls, _ := m.Get(ctx, operator, id); ls.State != api.StateRunning {
			t.Fatalf("%s was affected: %+v", id, ls)
		}
	}
	// The nested lease hitting its own limit ends alone.
	c := start(t, m, node(b), "", gib)
	f.OOM(c)
	if term := wait(t, m, c); term.Code != api.CodeOOMLeaseLimit {
		t.Fatalf("c: %+v", term)
	}
	if ls, _ := m.Get(ctx, operator, b); ls.State != api.StateRunning {
		t.Fatalf("b died with its child: %+v", ls)
	}
}

// watching wraps the fake backend with a holder watch the test controls.
type watching struct {
	*backend.Fake
	gone chan struct{}
	err  error
}

func (w *watching) WatchHolder(context.Context, int) (<-chan struct{}, error) { return w.gone, w.err }

// F1 and F15 at the manager level: when the process that started a lead
// dies, the whole tree is reaped with 502.
func TestHolderDeathReapsLead(t *testing.T) {
	w := &watching{Fake: backend.NewFake(), gone: make(chan struct{})}
	m, err := NewManager(ctx, Config{Backend: w, Capacity: 32 * gib})
	if err != nil {
		t.Fatal(err)
	}
	holder := Caller{UID: 501, PID: 4242}
	lead := start(t, m, holder, "", 8*gib)
	kid := start(t, m, node(lead), "", gib)
	w.AddProcs(kid, 3)

	close(w.gone) // the terminal is closed; "subactl run" dies
	term := wait(t, m, lead)
	if term.Code != api.CodeParentDied || term.Evidence["holder_pid"] != 4242 {
		t.Fatalf("lead: %+v", term)
	}
	if kt := wait(t, m, kid); kt.Code != api.CodeParentDied {
		t.Fatalf("kid: %+v", kt)
	}
	if refs, _ := w.List(ctx); len(refs) != 0 {
		t.Fatalf("acgroups left: %v", refs)
	}
	// A holder that is already gone cannot be given a lead.
	w.err = errors.New("no such process")
	_, err = m.Acquire(ctx, holder, api.AcquireRequest{Envelope: api.Envelope{Memory: gib}})
	wantStatus(t, err, http.StatusConflict, api.ErrConflict, "")
	time.Sleep(20 * time.Millisecond)
	if h := m.Host(); h.Allocated != 0 {
		t.Fatalf("a lead without a holder kept %d bytes", h.Allocated)
	}
}

// F10 for 408 and 414: the wall-time and output limits end the lease with
// their own codes, and a raised limit takes effect.
func TestWallAndOutputLimits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, f := newManager(t)
		acquire := func(e api.Envelope) api.LeaseID {
			ls, err := m.Acquire(ctx, operator, api.AcquireRequest{Envelope: e})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.Spawn(ctx, operator, ls.ID, api.SpawnRequest{Argv: []string{"agent"}}); err != nil {
				t.Fatal(err)
			}
			return ls.ID
		}
		began := time.Now()
		slow := acquire(api.Envelope{Memory: gib, Wall: 30})
		patient := acquire(api.Envelope{Memory: gib, Wall: 30})
		chatty := acquire(api.Envelope{Memory: gib, Output: 1000})
		quiet := acquire(api.Envelope{Memory: gib, Output: 1000})
		f.AddProcs(slow, 2)

		// After ten seconds the planner gives one lease more time.
		time.Sleep(10 * time.Second)
		wall := int64(90)
		if _, err := m.Resize(ctx, operator, patient, api.EnvelopePatch{Wall: &wall}); err != nil {
			t.Fatal(err)
		}
		f.SetOutput(quiet, 1000) // exactly at the limit is allowed
		f.SetOutput(chatty, 1001)

		term, _ := m.waitReaped(ctx, chatty)
		if term.Code != api.CodeOutputExceeded || term.Dimension != "output" || time.Since(began) > 12*time.Second {
			t.Fatalf("chatty after %v: %+v", time.Since(began), term)
		}
		term, _ = m.waitReaped(ctx, slow)
		if term.Code != api.CodeWallExceeded || term.Dimension != "wall" || term.Evidence["reaped_pids"] != 3 {
			t.Fatalf("slow: %+v", term)
		}
		if took := time.Since(began); took < 30*time.Second || took > 31*time.Second {
			t.Fatalf("the wall limit fired after %v, want 30s plus at most one tick", took)
		}
		term, _ = m.waitReaped(ctx, patient)
		if took := time.Since(began); term.Code != api.CodeWallExceeded || took < 90*time.Second || took > 91*time.Second {
			t.Fatalf("patient after %v: %+v", took, term)
		}
		if ls, _ := m.Get(ctx, operator, quiet); ls.State != api.StateRunning {
			t.Fatalf("a lease at its output limit was killed: %+v", ls)
		}
		m.Kill(ctx, operator, quiet)
	})
}
