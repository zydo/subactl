// Package daemon is subactld: the lease lifecycle on top of the ledger and
// a backend, and the HTTP API in front of it.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/internal/audit"
	"github.com/zydo/subactl/internal/backend"
	"github.com/zydo/subactl/internal/capacity"
	"github.com/zydo/subactl/internal/ledger"
	"github.com/zydo/subactl/internal/telemetry"
)

// Config configures a Manager.
type Config struct {
	Backend backend.Backend
	// Capacity is the effective capacity in bytes when Host is nil, and a
	// cap on the computed capacity when Host is set (zero: no cap).
	Capacity int64
	MemTotal int64
	Reserve  int64
	// Host reads the machine once per tick. With it the effective capacity
	// follows external load and the safety net is armed; without it the
	// capacity is fixed.
	Host capacity.Reader
	// NearLimit is the share of the hard limit at which NEAR_LIMIT is sent
	// (default 0.9).
	NearLimit float64
	// SafetyWindow is what planners get between the warning and the first
	// safety-net termination (default 5s).
	SafetyWindow time.Duration
	// StallNotice is how long a working lease may be silent before its
	// planner is told (default 10m); MaxIdle is when it is reaped (default
	// 30m). MaxIdleDone is how long a lease that reported done may stay
	// alive (default 60s).
	StallNotice, MaxIdle, MaxIdleDone time.Duration
	// Audit receives the audit records; nil keeps none.
	Audit *audit.Log
	// AuditDir is where traces of leases no longer in memory are read from.
	AuditDir string
	// Suggest holds the history behind quota suggestions; nil starts empty.
	Suggest *audit.Suggester
	// DefaultSuggestion is suggested for a kind without enough history
	// (default 2GiB).
	DefaultSuggestion int64
	// DefaultGrace is how long terminate waits before SIGTERM (default 30s).
	DefaultGrace time.Duration
	// TermWait is how long terminate waits after SIGTERM (default 10s).
	TermWait time.Duration
	// Tick is how often wall-time and output limits are checked (default 1s).
	Tick time.Duration
	// EscapeScan is how often escaped processes are looked for (default 5s).
	EscapeScan time.Duration
	Log        *slog.Logger
}

// Caller is who is making a request. A caller inside an acgroup is that
// node; any other caller is an operator, authorised by uid.
type Caller struct {
	Lease api.LeaseID // empty for an operator
	UID   int
	Admin bool
	// PID is the calling process. An operator that creates a lead becomes
	// its holder: when that process dies, the lead is reaped.
	PID int
}

// Error is an API error with its HTTP status.
type Error struct {
	Status int
	API    api.Error
}

func (e *Error) Error() string { return e.API.Error() }

func invalid(field, expected, example, format string, a ...any) *Error {
	return &Error{http.StatusBadRequest, api.Error{
		Code: api.ErrInvalidArgument, Message: fmt.Sprintf(format, a...),
		Field: field, Expected: expected, Example: example,
	}}
}

func apiErr(status int, code, format string, a ...any) *Error {
	return &Error{status, api.Error{Code: code, Message: fmt.Sprintf(format, a...)}}
}

type runtime struct {
	group backend.ACGroup
	proc  backend.Proc
	done  chan struct{} // closed once the lease is reaped
	// pending is the code requested by terminate. If the main process exits
	// while it is set, the exit is attributed to that terminate.
	pending int
	step    string
	started time.Time // when the main process was spawned
	// wantTTY is set at acquire for a lead that asked for a terminal; tty
	// is the relay once the main process runs.
	wantTTY bool
	tty     *ttyRelay
}

// Manager owns every lease from acquire to reaped.
type Manager struct {
	cfg Config
	be  backend.Backend
	led *ledger.Ledger
	log *slog.Logger

	// abandoned makes the manager inert, as if its process had been killed.
	// Tests set it to model a daemon crash inside one process.
	abandoned atomic.Bool

	obs *observer
	ctl *control

	admit sync.Mutex // serialises ledger-plus-backend changes to reservations
	mu    sync.Mutex
	rt    map[api.LeaseID]*runtime
}

// NewManager starts fail-closed: every acgroup the backend still reports is
// left over from a previous daemon, so it is killed as a whole and destroyed
// before any request is served. Running leases are never adopted.
func NewManager(ctx context.Context, cfg Config) (*Manager, error) {
	if cfg.DefaultGrace == 0 {
		cfg.DefaultGrace = 30 * time.Second
	}
	if cfg.TermWait == 0 {
		cfg.TermWait = 10 * time.Second
	}
	if cfg.NearLimit == 0 {
		cfg.NearLimit = 0.9
	}
	if cfg.SafetyWindow == 0 {
		cfg.SafetyWindow = 5 * time.Second
	}
	if cfg.StallNotice == 0 {
		cfg.StallNotice = 10 * time.Minute
	}
	if cfg.MaxIdle == 0 {
		cfg.MaxIdle = 30 * time.Minute
	}
	if cfg.MaxIdleDone == 0 {
		cfg.MaxIdleDone = time.Minute
	}
	if cfg.DefaultSuggestion == 0 {
		cfg.DefaultSuggestion = 2 << 30
	}
	if cfg.Tick == 0 {
		cfg.Tick = time.Second
	}
	if cfg.EscapeScan == 0 {
		cfg.EscapeScan = 5 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	m := &Manager{
		cfg: cfg, be: cfg.Backend, log: cfg.Log,
		led: ledger.New(cfg.Capacity), rt: map[api.LeaseID]*runtime{},
	}
	m.obs, m.ctl = newObserver(cfg), newControl()
	if cfg.Host != nil {
		// Start from a real reading rather than from the bare cap.
		if h, err := cfg.Host.Read(); err == nil {
			m.led.SetEffectiveCapacity(capacity.Effective(cfg.Capacity, h, cfg.Reserve, capacity.External(h, 0)))
			m.obs.host, m.obs.external = h, capacity.External(h, 0)
		}
	}
	if err := m.failClosed(ctx); err != nil {
		return nil, err
	}
	go m.tickLoop(ctx)
	if sc, ok := m.be.(backend.EscapeScanner); ok {
		go m.escapeLoop(ctx, sc)
	}
	return m, nil
}

// escapeLoop looks every few seconds for processes that left their acgroup
// but still carry its marker. Each is killed, and the lease it came from
// ends with 451. The loop stops when ctx is cancelled.
func (m *Manager) escapeLoop(ctx context.Context, sc backend.EscapeScanner) {
	tick := time.NewTicker(m.cfg.EscapeScan)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		found, err := sc.ScanEscapes(ctx)
		if err != nil {
			m.log.Warn("escape scan failed", "err", err)
			continue
		}
		for _, e := range found {
			_ = syscall.Kill(e.Pid, syscall.SIGKILL)
			m.log.Warn("escaped process killed", "pid", e.Pid, "lease", e.Lease)
			m.publish(telemetry.EventEscape, e.Lease, map[string]any{"pid": e.Pid})
			// The lease may be long gone; then the kill is all there is to do.
			m.reap(e.Lease, api.CodeEscapeDetected, map[string]any{"escaped_pid": e.Pid})
		}
	}
}

func (m *Manager) failClosed(ctx context.Context) error {
	refs, err := m.be.List(ctx)
	if err != nil {
		return fmt.Errorf("fail-closed cleanup: list acgroups: %w", err)
	}
	for _, ref := range refs {
		rep, err := m.be.KillTree(ctx, backend.Handle(ref))
		m.log.Warn("fail-closed: killed leftover acgroup", "lease", ref.Lease,
			"code", api.CodeSubactlInternal, "killed", rep.Killed, "remaining", rep.Remaining, "err", err)
	}
	// Children must go before parents and List gives no order, so sweep
	// until a pass makes no progress.
	for len(refs) > 0 {
		var left []backend.ACGroupRef
		var last error
		for _, ref := range refs {
			if err := m.be.Destroy(ctx, backend.Handle(ref)); err != nil {
				left, last = append(left, ref), err
			}
		}
		if len(left) == len(refs) {
			return fmt.Errorf("fail-closed cleanup: %d acgroups could not be destroyed: %w", len(left), last)
		}
		refs = left
	}
	return nil
}

func newID() api.LeaseID {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return api.LeaseID("ls_" + hex.EncodeToString(b[:]))
}

func (m *Manager) runtime(id api.LeaseID) *runtime {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rt[id]
}

// Host returns the machine-wide view.
func (m *Manager) Host() api.Host {
	eff, alloc, shrunk := m.led.Totals()
	h := api.Host{
		Backend: m.be.Name(), MemBasis: m.be.MemBasis(),
		MemTotal: m.cfg.MemTotal, Reserve: m.cfg.Reserve,
		EffectiveCapacity: eff, Allocated: alloc, CapacityShrunk: shrunk,
		Capabilities: m.be.Capabilities(),
	}
	if m.cfg.Host != nil {
		m.obs.mu.Lock()
		h.MemTotal, h.External = m.obs.host.Total, m.obs.external
		m.obs.mu.Unlock()
	}
	return h
}

// resolve finds the lease named by id (or "self") and checks that the
// caller may see it: a node sees its own subtree, an operator sees the lead
// trees it started.
func (m *Manager) resolve(c Caller, id api.LeaseID) (api.Lease, error) {
	if id == api.Self {
		if c.Lease == "" {
			return api.Lease{}, invalid("id", "a lease id", "ls_7f3a9c21",
				"\"self\" names the caller's own lease, but the caller is an operator outside every lease")
		}
		id = c.Lease
	}
	ls, ok := m.led.Get(id)
	if !ok {
		return api.Lease{}, apiErr(http.StatusNotFound, api.ErrNotFound, "no lease %s", id)
	}
	if c.Lease != "" {
		if !m.led.InSubtree(c.Lease, id) {
			return api.Lease{}, apiErr(http.StatusForbidden, api.ErrOutOfSubtree, "lease %s is outside the subtree of %s", id, c.Lease)
		}
	} else if !c.Admin && ls.OwnerUID != c.UID {
		return api.Lease{}, apiErr(http.StatusForbidden, api.ErrOutOfSubtree, "lease %s belongs to another user", id)
	}
	return ls, nil
}

var kindRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func validateEnvelope(e api.Envelope) *Error {
	switch {
	case e.Memory <= 0:
		return invalid("envelope.memory", "bytes > 0", "8589934592", "memory must be positive, got %d", e.Memory)
	case e.MemorySelf < 0 || e.MemorySelf > e.Memory:
		return invalid("envelope.memory_self", "0 <= memory_self <= memory", "1073741824", "memory_self %d is outside [0, %d]", e.MemorySelf, e.Memory)
	case e.CPU < 0:
		return invalid("envelope.cpu", "cores >= 0 (0 is unlimited)", "2", "cpu must not be negative")
	case e.Pids < 0:
		return invalid("envelope.pids", "count >= 0 (0 is unlimited)", "512", "pids must not be negative")
	case e.Wall < 0:
		return invalid("envelope.wall", "seconds >= 0 (0 is unlimited)", "1800", "wall must not be negative")
	case e.Output < 0:
		return invalid("envelope.output", "bytes >= 0 (0 is unlimited)", "104857600", "output must not be negative")
	}
	return nil
}

// Acquire admits a lease and creates its acgroup. Nothing runs in it yet.
func (m *Manager) Acquire(ctx context.Context, c Caller, req api.AcquireRequest) (api.Lease, error) {
	parent := req.Parent
	if c.Lease != "" && parent == "" {
		parent = c.Lease // a node can only create leases under itself
	}
	p := ledger.Params{Kind: req.Kind, Envelope: req.Envelope, KillOrder: req.KillOrder, OnNearLimit: req.OnNearLimit, DryRun: req.DryRun}
	if parent != "" {
		pl, err := m.resolve(c, parent)
		if err != nil {
			return api.Lease{}, err
		}
		p.Parent = pl.ID
		if req.UID != nil {
			return api.Lease{}, invalid("uid", "omitted for a child lease", "", "a child lease runs as its parent's uid")
		}
		if req.TTY {
			return api.Lease{}, invalid("tty", "false for a child lease", "false", "only a lead gets a pty; child agents are headless")
		}
	} else {
		p.OwnerUID, p.UID = c.UID, c.UID
		if req.UID != nil {
			p.UID = *req.UID
		}
		switch {
		case p.UID == 0:
			return api.Lease{}, invalid("uid", "a non-root uid", "1000", "an agent never runs as root; name the uid the lead should run as")
		case p.UID != c.UID && c.UID != 0 && !c.Admin:
			return api.Lease{}, invalid("uid", "your own uid", fmt.Sprint(c.UID), "only root may start a lead as another user")
		}
		if p.Envelope.Memory == 0 {
			if p.Envelope.Memory = m.led.DefaultLeadMemory(); p.Envelope.Memory == 0 {
				return api.Lease{}, &Error{http.StatusConflict, api.Error{Code: api.ErrRejected, Reason: api.ReasonMemBudget,
					Message: "no unallocated capacity is left for a default lead envelope"}}
			}
		}
	}
	if req.Kind != "" && !kindRE.MatchString(req.Kind) {
		return api.Lease{}, invalid("kind", "lower-case letters, digits, '-' and '_', at most 32", "worker", "invalid kind %q", req.Kind)
	}
	if err := validateEnvelope(p.Envelope); err != nil {
		return api.Lease{}, err
	}
	if g := req.OnNearLimit; g != nil && g.GrowTo <= p.Envelope.Memory {
		return api.Lease{}, invalid("on_near_limit.grow_to", "bytes greater than envelope.memory", fmt.Sprint(p.Envelope.Memory*2), "grow_to must exceed the initial memory limit")
	}
	if r := req.Require; r != nil {
		caps := m.be.Capabilities()
		for _, d := range []struct {
			name      string
			want, got api.Level
		}{{"memory", r.Memory, caps.Memory}, {"cpu", r.CPU, caps.CPU}, {"pids", r.Pids, caps.Pids}, {"kill_tree", r.KillTree, caps.KillTree}} {
			if d.want == "" {
				continue
			}
			if !d.want.Valid() {
				return api.Lease{}, invalid("require."+d.name, "hard, soft, monitor_only or unsupported", "hard", "unknown enforcement level %q", d.want)
			}
			if !d.got.Satisfies(d.want) {
				return api.Lease{}, &Error{http.StatusConflict, api.Error{Code: api.ErrRejected, Reason: api.ReasonEnforcementUnavailable,
					Message: fmt.Sprintf("require.%s is %s, but backend %s only reaches %s", d.name, d.want, m.be.Name(), d.got)}}
			}
		}
	}

	m.admit.Lock()
	defer m.admit.Unlock()
	p.ID = newID()
	for _, taken := m.led.Get(p.ID); taken; _, taken = m.led.Get(p.ID) {
		p.ID = newID()
	}
	ls, err := m.led.Acquire(p)
	if err != nil {
		return api.Lease{}, m.refused(err)
	}
	if req.DryRun {
		return ls, nil
	}
	g, err := m.be.Create(ctx, ls.ID, backend.ACGroupRef{Lease: ls.Parent}, ls.Envelope)
	if err != nil {
		m.led.Abort(ls.ID)
		return api.Lease{}, apiErr(http.StatusInternalServerError, api.ErrInternal, "create acgroup: %v", err)
	}
	rt := &runtime{group: g, done: make(chan struct{}), wantTTY: req.TTY}
	m.mu.Lock()
	m.rt[ls.ID] = rt
	m.mu.Unlock()
	if ls.Parent == "" {
		if err := m.watchHolder(ls.ID, rt, c.PID); err != nil {
			go m.reap(ls.ID, api.CodeParentDied, map[string]any{"cause": "the holder exited before the lead was created", "holder_pid": c.PID})
			return api.Lease{}, apiErr(http.StatusConflict, api.ErrConflict, "the process that asked for the lead has already exited: %v", err)
		}
	}
	m.log.Info("lease admitted", "lease", ls.ID, "parent", ls.Parent, "kind", ls.Kind, "memory", ls.Envelope.Memory)
	who := fmt.Sprintf("operator uid %d", c.UID)
	if c.Lease != "" {
		who = "lease " + string(c.Lease)
	}
	m.auditLease(audit.TypeAcquire, ls, func(r *audit.Record) { r.Caller, r.Envelope = who, &ls.Envelope })
	m.publish(telemetry.EventState, ls.ID, map[string]any{"state": api.StateAdmitted})
	return ls, nil
}

// watchHolder ties a lead to the operator process that created it. A
// nested lease needs no watch: its holder is its parent's main process,
// whose exit already reaps the subtree. Backends that cannot watch a
// process exactly do not get a weaker substitute.
func (m *Manager) watchHolder(id api.LeaseID, rt *runtime, pid int) error {
	hw, ok := m.be.(backend.HolderWatcher)
	if !ok || pid <= 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	gone, err := hw.WatchHolder(ctx, pid)
	if err != nil {
		cancel()
		return err
	}
	go func() {
		defer cancel()
		select {
		case <-gone:
			m.reap(id, api.CodeParentDied, map[string]any{"cause": "the holder process exited", "holder_pid": pid})
		case <-rt.done:
		}
	}()
	return nil
}

// refused turns a ledger error into an API error and counts a rejection.
func (m *Manager) refused(err error) error {
	var rej *ledger.Rejection
	if errors.As(err, &rej) {
		m.obs.metrics.Reject(rej.Reason)
	}
	return ledgerErr(err)
}

func ledgerErr(err error) error {
	var rej *ledger.Rejection
	if errors.As(err, &rej) {
		return &Error{http.StatusConflict, api.Error{Code: api.ErrRejected, Reason: rej.Reason, Message: rej.Message}}
	}
	return apiErr(http.StatusInternalServerError, api.ErrInternal, "%v", err)
}

// Spawn starts the main process of an admitted lease inside its acgroup.
func (m *Manager) Spawn(ctx context.Context, c Caller, id api.LeaseID, req api.SpawnRequest) (api.SpawnResponse, error) {
	ls, err := m.resolve(c, id)
	if err != nil {
		return api.SpawnResponse{}, err
	}
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return api.SpawnResponse{}, invalid("argv", "a non-empty command", `["mini-agent","--task","t.md"]`, "argv is empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.rt[ls.ID]
	if rt == nil || rt.proc != nil || m.led.Running(ls.ID) != nil {
		return api.SpawnResponse{}, apiErr(http.StatusConflict, api.ErrConflict, "lease %s is not waiting for its main process; a lease has exactly one", ls.ID)
	}
	proc, err := m.be.Spawn(ctx, rt.group, backend.Cmd{Argv: req.Argv, Env: req.Env, Dir: req.Dir, UID: ls.UID, TTY: rt.wantTTY})
	if err != nil {
		// The lease is running in the ledger but has no process: reap it.
		go m.reap(ls.ID, api.CodeSubactlInternal, map[string]any{"spawn_error": err.Error()})
		return api.SpawnResponse{}, apiErr(http.StatusInternalServerError, api.ErrInternal, "spawn: %v", err)
	}
	rt.proc, rt.started = proc, time.Now()
	if tp, ok := proc.(backend.TTYProc); ok && tp.TTY() != nil {
		rt.tty = newTTYRelay(tp.TTY())
	} else if rt.wantTTY {
		// Honest about it: this backend has no terminal to give.
		m.log.Warn("the backend cannot give this lead a terminal; it runs headless", "lease", ls.ID)
	}
	go m.onMainExit(ls.ID, proc.Wait)
	go m.publish(telemetry.EventState, ls.ID, map[string]any{"state": api.StateRunning, "pid": proc.Pid()})
	return api.SpawnResponse{Pid: proc.Pid()}, nil
}

// onMainExit runs when the main process of a lease exits, for any reason.
// The lease is reaped at once: no descendant survives its main process.
func (m *Manager) onMainExit(id api.LeaseID, wait func() backend.ExitStatus) {
	st := wait()
	ev := map[string]any{"exit_code": st.Code}
	code := api.CodeCompleted
	if st.Signal != 0 {
		ev["signal"] = st.Signal
	}
	if st.Signal != 0 || st.Code != 0 {
		// Not our signal and not a clean exit: an external abort or a crash.
		code = api.CodeExternalAbort
	}
	m.mu.Lock()
	if rt := m.rt[id]; rt != nil && rt.pending != 0 {
		code, ev["step"] = rt.pending, rt.step
	}
	m.mu.Unlock()
	// A memory limit is a fact that outranks every other reading of the
	// exit. The limit that was hit may be an ancestor's: then that ancestor
	// ends with 413 and this lease is reaped with it, as 502.
	if st.Signal != 0 {
		if hit, n := m.oomLimitHit(id); hit != "" {
			m.reap(hit, api.CodeOOMLeaseLimit, map[string]any{"oom_events": n})
		}
	}
	m.reap(id, code, ev)
}

// oomLimitHit looks from the lease upwards for the nearest acgroup whose
// own memory limit was hit.
func (m *Manager) oomLimitHit(id api.LeaseID) (api.LeaseID, int64) {
	for id != "" {
		rt := m.runtime(id)
		if rt == nil {
			break
		}
		if u, err := m.be.Sample(context.Background(), rt.group); err == nil && u.OOMEvents > 0 {
			return id, u.OOMEvents
		}
		ls, ok := m.led.Get(id)
		if !ok {
			break
		}
		id = ls.Parent
	}
	return "", 0
}

// reap ends a lease and its whole subtree. It is safe to call from several
// goroutines for the same lease: the ledger lets only the first cause win.
func (m *Manager) reap(id api.LeaseID, code int, evidence map[string]any) {
	if m.abandoned.Load() {
		return
	}
	// admit is held so that every marked lease has finished Acquire and has
	// its runtime; a child caught mid-creation is either complete or absent.
	m.admit.Lock()
	marked := m.led.BeginReap(id, code)
	m.admit.Unlock()
	if len(marked) == 0 {
		return
	}
	ctx := context.Background()
	root := m.runtime(id)
	peaks := map[api.LeaseID]api.Usage{}
	for _, mk := range marked {
		if u, err := m.be.Sample(ctx, m.runtime(mk.ID).group); err == nil {
			peaks[mk.ID] = u
		}
	}
	rep, killErr := m.be.KillTree(ctx, root.group)
	for _, mk := range marked { // children before parents
		rt := m.runtime(mk.ID)
		ls, _ := m.led.Get(mk.ID)
		// Children that were already being reaped on their own must finish
		// first; they are not in marked.
		for _, child := range ls.Children {
			if crt := m.runtime(child); crt != nil {
				<-crt.done
			}
		}
		ev := map[string]any{}
		final := mk.Code
		if mk.ID == id {
			for k, v := range evidence {
				ev[k] = v
			}
			ev["reaped_pids"] = rep.Killed
			if killErr != nil || rep.Remaining > 0 {
				final = api.CodeReapIncomplete
				ev["remaining_pids"], ev["intended_code"] = rep.Remaining, mk.Code
				if killErr != nil {
					ev["kill_error"] = killErr.Error()
				}
			}
		} else {
			ev["cause"] = fmt.Sprintf("ancestor %s ended with %d %s", id, code, api.CodeName(code))
		}
		if err := m.be.Destroy(ctx, rt.group); err != nil && final != api.CodeReapIncomplete {
			final = api.CodeReapIncomplete
			ev["destroy_error"], ev["intended_code"] = err.Error(), mk.Code
		}
		term := m.record(ls, final, peaks[mk.ID], ev)
		if err := m.led.Finish(mk.ID, term); err != nil {
			m.log.Error("ledger refused to finish a lease", "lease", mk.ID, "err", err)
		}
		m.mu.Lock()
		delete(m.rt, mk.ID)
		m.mu.Unlock()
		if rt.tty != nil {
			// Every process is gone, so the terminal reports end of file
			// once its last output has been read. Give the relay a moment
			// to deliver that before the master end is closed.
			select {
			case <-rt.tty.closed:
			case <-time.After(500 * time.Millisecond):
			}
			rt.tty.close()
		}
		m.ended(ls, term)
		m.forget(ls)
		close(rt.done)
		m.log.Info("lease reaped", "lease", mk.ID, "code", final, "name", term.Name)
	}
}

func (m *Manager) record(ls api.Lease, code int, u api.Usage, ev map[string]any) api.Termination {
	t := api.Termination{
		Code: code, Name: api.CodeName(code), Lease: ls.ID, Kind: ls.Kind, Parent: ls.Parent,
		MemBasis: m.be.MemBasis(), Peak: u.MemoryPeak, Limit: ls.Envelope.Memory,
		Enforcement: m.be.Capabilities().Memory, Evidence: ev, At: time.Now(),
	}
	who := "lease " + string(ls.ID)
	if ls.Kind != "" {
		who = ls.Kind + " " + string(ls.ID)
	}
	switch code {
	case api.CodeOOMLeaseLimit:
		t.Dimension = "memory"
		t.Message = fmt.Sprintf("%s exceeded its memory hard limit of %s and was killed as a whole.", who, api.FormatBytes(ls.Envelope.Memory))
	case api.CodeWallExceeded:
		t.Dimension = "wall"
		t.Message = fmt.Sprintf("%s ran longer than its wall-time limit of %ds and was killed as a whole.", who, ls.Envelope.Wall)
	case api.CodeOutputExceeded:
		t.Dimension = "output"
		t.Message = fmt.Sprintf("%s wrote more than its output limit of %s and was killed as a whole.", who, api.FormatBytes(ls.Envelope.Output))
	case api.CodeEscapeDetected:
		t.Message = who + " had a process outside its acgroup; the process was killed and the lease ended."
	case api.CodeCompleted:
		t.Message = who + " completed: its main process exited with status 0."
	case api.CodeReleased:
		t.Message = who + " was released by its planner."
	case api.CodeKilled:
		t.Message = who + " was terminated by its planner."
	case api.CodeParentDied:
		t.Message = who + " was reaped because an ancestor ended."
	case api.CodeExternalAbort:
		t.Message = who + " ended abnormally: its main process was killed from outside or crashed."
	case api.CodeReapIncomplete:
		t.Message = who + " could not be fully reaped: processes remain in its acgroup."
	case api.CodeSubactlInternal:
		t.Message = who + " was ended by an internal subactld error."
	default:
		t.Message = who + " ended with " + t.Name + "."
	}
	return t
}

// withUsage attaches a fresh sample to a live lease.
func (m *Manager) withUsage(ctx context.Context, ls api.Lease) api.Lease {
	ls.Scratch = m.scratchDir(ls.ID)
	if rt := m.runtime(ls.ID); rt != nil {
		if u, err := m.be.Sample(ctx, rt.group); err == nil {
			ls.Usage = &u
		}
	}
	return ls
}

// Get returns one lease.
func (m *Manager) Get(ctx context.Context, c Caller, id api.LeaseID) (api.Lease, error) {
	ls, err := m.resolve(c, id)
	if err != nil {
		return api.Lease{}, err
	}
	return m.withUsage(ctx, ls), nil
}

// List returns whole trees in pre-order: a node's own subtree, or the lead
// trees an operator may see, optionally narrowed to one lead.
func (m *Manager) List(ctx context.Context, c Caller, lead api.LeaseID) ([]api.Lease, error) {
	var roots []api.LeaseID
	switch {
	case c.Lease != "":
		roots = []api.LeaseID{c.Lease}
	case lead != "":
		ls, err := m.resolve(c, lead)
		if err != nil {
			return nil, err
		}
		roots = []api.LeaseID{ls.Lead}
	default:
		roots = m.led.Leads()
	}
	out := []api.Lease{}
	for _, r := range roots {
		tree := m.led.Subtree(r)
		if len(tree) == 0 || (c.Lease == "" && !c.Admin && tree[0].OwnerUID != c.UID) {
			continue
		}
		for _, ls := range tree {
			out = append(out, m.withUsage(ctx, ls))
		}
	}
	return out, nil
}

// Available returns the remaining budget of a subtree.
func (m *Manager) Available(c Caller, id api.LeaseID, kinds ...string) (api.Available, error) {
	ls, err := m.resolve(c, id)
	if err != nil {
		return api.Available{}, err
	}
	a, _ := m.led.Available(ls.ID)
	a.MemBasis = m.be.MemBasis()
	// Suggestions are shared across leads on purpose: more history, better
	// numbers. Kinds the caller names are always answered, with the default
	// if need be.
	for _, k := range append(m.obs.suggest.Kinds(), kinds...) {
		if k != "" {
			a.Suggest[k] = m.obs.suggest.Suggest(k)
		}
	}
	return a, nil
}

// Resize adjusts the envelope of a live lease. A node may resize its
// descendants but never itself; it must ask its parent.
func (m *Manager) Resize(ctx context.Context, c Caller, id api.LeaseID, p api.EnvelopePatch) (api.Lease, error) {
	ls, err := m.resolve(c, id)
	if err != nil {
		return api.Lease{}, err
	}
	if ls.ID == c.Lease {
		return api.Lease{}, apiErr(http.StatusForbidden, api.ErrSelfResize, "a node cannot resize itself; use resize-request to ask the parent")
	}
	return m.applyResize(ctx, ls, p, "planner")
}

// applyResize changes a lease's envelope in the ledger and the backend.
// by says who asked: a planner, or a rule the planner declared.
func (m *Manager) applyResize(ctx context.Context, ls api.Lease, p api.EnvelopePatch, by string) (api.Lease, error) {
	next := ls.Envelope
	for _, f := range []struct {
		src *int64
		dst *int64
	}{{p.Memory, &next.Memory}, {p.MemorySelf, &next.MemorySelf}, {p.Pids, &next.Pids}, {p.Wall, &next.Wall}, {p.Output, &next.Output}} {
		if f.src != nil {
			*f.dst = *f.src
		}
	}
	if p.CPU != nil {
		next.CPU = *p.CPU
	}
	if err := validateEnvelope(next); err != nil {
		return api.Lease{}, err
	}
	m.admit.Lock()
	defer m.admit.Unlock()
	rt := m.runtime(ls.ID)
	if rt == nil {
		return api.Lease{}, apiErr(http.StatusConflict, api.ErrConflict, "lease %s has ended", ls.ID)
	}
	var usage int64
	if u, err := m.be.Sample(ctx, rt.group); err == nil {
		usage = u.Memory
	}
	out, err := m.led.Resize(ls.ID, p, usage)
	if err != nil {
		return api.Lease{}, m.refused(err)
	}
	if p.DryRun {
		return out, nil
	}
	if err := m.be.SetLimits(ctx, rt.group, out.Envelope); err != nil {
		// Put the old envelope back. admit is held, so the room is still there.
		old := ls.Envelope
		back := api.EnvelopePatch{Memory: &old.Memory, MemorySelf: &old.MemorySelf, CPU: &old.CPU, Pids: &old.Pids, Wall: &old.Wall, Output: &old.Output}
		if _, rerr := m.led.Resize(ls.ID, back, 0); rerr != nil {
			m.log.Error("could not restore envelope after a failed resize", "lease", ls.ID, "err", rerr)
		}
		return api.Lease{}, apiErr(http.StatusInternalServerError, api.ErrInternal, "apply limits: %v", err)
	}
	m.log.Info("lease resized", "lease", ls.ID, "memory", out.Envelope.Memory, "by", by)
	m.auditLease(audit.TypeResize, out, func(r *audit.Record) { r.Caller, r.Envelope = by, &out.Envelope })
	m.publish(telemetry.EventResized, ls.ID, map[string]any{"by": by, "memory": out.Envelope.Memory, "previous_memory": ls.Envelope.Memory})
	return m.withUsage(ctx, out), nil
}

// waitReaped blocks until the lease is reaped or ctx ends.
func (m *Manager) waitReaped(ctx context.Context, id api.LeaseID) (api.Termination, bool) {
	if rt := m.runtime(id); rt != nil {
		select {
		case <-rt.done:
		case <-ctx.Done():
			return api.Termination{}, false
		}
	}
	// No runtime means the lease was already reaped.
	if ls, ok := m.led.Get(id); ok && ls.Termination != nil {
		return *ls.Termination, true
	}
	return api.Termination{}, false
}

// Kill ends the whole tree at once, recorded as 499.
func (m *Manager) Kill(ctx context.Context, c Caller, id api.LeaseID) (api.Termination, error) {
	ls, err := m.resolve(c, id)
	if err != nil {
		return api.Termination{}, err
	}
	m.reap(ls.ID, api.CodeKilled, map[string]any{"step": "kill"})
	return m.terminated(ctx, ls.ID)
}

func (m *Manager) terminated(ctx context.Context, id api.LeaseID) (api.Termination, error) {
	t, ok := m.waitReaped(ctx, id)
	if !ok {
		return api.Termination{}, apiErr(http.StatusGatewayTimeout, api.ErrInternal, "gave up waiting for lease %s to be reaped; it is still being terminated", id)
	}
	return t, nil
}

// Terminate ends a lease gracefully: wait for it to exit by itself within
// the grace period, then SIGTERM its main process, then kill the whole
// tree. The ladder runs to the end even if the caller goes away.
//
// Step one puts TERMINATE_REQUESTED, with the reason and the deadline, in
// the lease's inbox; with recursive, in every descendant's too. An agent
// with an adapter saves its progress and exits; one without never sees it.
func (m *Manager) Terminate(ctx context.Context, c Caller, id api.LeaseID, req api.TerminateRequest) (api.Termination, error) {
	ls, err := m.resolve(c, id)
	if err != nil {
		return api.Termination{}, err
	}
	code := req.Code
	if code == 0 {
		code = api.CodeKilled
	}
	if code != api.CodeReleased && code != api.CodeKilled {
		return api.Termination{}, invalid("code", "204 (released) or 499 (killed)", "204", "terminate cannot record code %d", req.Code)
	}
	grace := m.cfg.DefaultGrace
	if g := req.GraceSeconds; g != nil {
		if *g < 0 || *g > 3600 {
			return api.Termination{}, invalid("grace_seconds", "0 to 3600", "30", "grace_seconds %v is out of range", *g)
		}
		grace = time.Duration(*g * float64(time.Second))
	}
	m.mu.Lock()
	rt := m.rt[ls.ID]
	start := rt != nil && rt.pending == 0
	var proc backend.Proc
	if start {
		rt.pending, rt.step, proc = code, "grace", rt.proc
	}
	m.mu.Unlock()
	if start {
		source := "operator"
		if c.Lease != "" {
			source = string(c.Lease)
		}
		deadline := time.Now().Add(grace)
		targets := []api.Lease{ls}
		if req.Recursive {
			targets = m.led.Subtree(ls.ID)
		}
		for _, t := range targets {
			if t.State == api.StateRunning {
				m.deliver(t.ID, api.MsgTerminateRequested, source, cleanText(req.Reason, maxReason), &deadline)
			}
		}
		go m.ladder(ls.ID, rt, proc, code, grace)
	}
	return m.terminated(ctx, ls.ID)
}

func (m *Manager) ladder(id api.LeaseID, rt *runtime, proc backend.Proc, code int, grace time.Duration) {
	setStep := func(s string) {
		m.mu.Lock()
		rt.step = s
		m.mu.Unlock()
	}
	if proc == nil { // admitted but never spawned: nothing to wait for
		m.reap(id, code, map[string]any{"step": "not_started"})
		return
	}
	if waitOr(rt.done, grace) {
		return
	}
	setStep("sigterm")
	_ = proc.Signal(syscall.SIGTERM)
	if waitOr(rt.done, m.cfg.TermWait) {
		return
	}
	setStep("kill_tree")
	m.reap(id, code, map[string]any{"step": "kill_tree"})
}

// waitOr reports whether done closed within d.
func waitOr(done <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// WaitChildren waits until every descendant of the lease has been reaped
// (and the lease itself when asked) and returns their termination records.
func (m *Manager) WaitChildren(ctx context.Context, c Caller, id api.LeaseID, req api.WaitChildrenRequest) (api.WaitChildrenResponse, error) {
	ls, err := m.resolve(c, id)
	if err != nil {
		return api.WaitChildrenResponse{}, err
	}
	if req.TimeoutSeconds < 0 {
		return api.WaitChildrenResponse{}, invalid("timeout_seconds", "seconds >= 0 (0 waits forever)", "600", "timeout_seconds must not be negative")
	}
	if req.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds*float64(time.Second)))
		defer cancel()
	}
	out := api.WaitChildrenResponse{Done: true, Results: []api.Termination{}}
	for _, l := range m.led.Subtree(ls.ID) {
		if l.ID == ls.ID && !req.Self {
			continue
		}
		t, ok := m.waitReaped(ctx, l.ID)
		if !ok {
			out.Done = false
			continue
		}
		out.Results = append(out.Results, t)
	}
	return out, nil
}

// Shutdown reaps every lease with 500 SUBACTL_INTERNAL. A daemon that stops
// leaves nothing running, exactly as a daemon that crashes leaves nothing
// running after its restart.
func (m *Manager) Shutdown() {
	for _, id := range m.led.Leads() {
		m.reap(id, api.CodeSubactlInternal, map[string]any{"step": "daemon_shutdown"})
	}
}
