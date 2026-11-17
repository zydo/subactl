package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/internal/audit"
	"github.com/zydo/subactl/internal/backend"
	"github.com/zydo/subactl/internal/capacity"
	"github.com/zydo/subactl/internal/metrics"
	"github.com/zydo/subactl/internal/telemetry"
)

const (
	ringSeconds   = 600  // in-memory samples per lease: ten minutes at one a second
	sampleEvery   = 5    // every fifth sample also goes to the audit log
	maxEvents     = 500  // events kept per lease for its trace
	keptTraces    = 1024 // traces of reaped leases kept in memory
	rearmFraction = 0.85 // NEAR_LIMIT re-arms once usage falls below this share
	topProcs      = 3    // processes listed in a termination's evidence
	topProcsFrom  = 0.75 // share of the limit from which they are listed at every sample
)

// leaseObs is what is observed about one lease. It is kept apart from the
// lease's runtime and under its own lock, so that observing can never
// hold up enforcement.
type leaseObs struct {
	ring   *telemetry.Ring
	events []api.Event
	near   bool // NEAR_LIMIT was sent and has not re-armed
	ticks  int
	term   *api.Termination
	// top are the processes using the most memory at the last sample that
	// was near the limit: by the time the kernel ends a lease for its
	// memory, they are gone and can no longer be listed.
	top   []api.Proc
	topAt time.Time
}

// observer is the manager's observation state.
type observer struct {
	bus     *telemetry.Bus
	metrics *metrics.Metrics
	audit   *audit.Log
	suggest *audit.Suggester

	mu     sync.Mutex
	leases map[api.LeaseID]*leaseObs
	ended  []api.LeaseID // reaped leases whose trace is still kept, oldest first

	// Host readings and the safety net, touched only by the tick loop
	// except for the snapshot under mu.
	host     capacity.Host
	external int64
	shrunk   bool
	net      capacity.Net
	// window is when the planner's grace period ends; zero when the net is
	// not tripped.
	window  time.Time
	killing bool
}

func newObserver(cfg Config) *observer {
	o := &observer{
		bus: telemetry.NewBus(), metrics: metrics.New(), audit: cfg.Audit, suggest: cfg.Suggest,
		leases: map[api.LeaseID]*leaseObs{}, net: capacity.Net{Need: 3},
	}
	if o.suggest == nil {
		o.suggest = audit.NewSuggester(cfg.DefaultSuggestion, cfg.Backend.MemBasis())
	}
	return o
}

func (o *observer) lease(id api.LeaseID) *leaseObs {
	l := o.leases[id]
	if l == nil {
		l = &leaseObs{ring: telemetry.NewRing(ringSeconds)}
		o.leases[id] = l
	}
	return l
}

// scope returns the lease with its ancestors, lead last.
func (m *Manager) scope(id api.LeaseID) (path []api.LeaseID, owner int, kind string) {
	for id != "" {
		ls, ok := m.led.Get(id)
		if !ok {
			break
		}
		if len(path) == 0 {
			kind = ls.Kind
		}
		path, owner = append(path, id), ls.OwnerUID
		id = ls.Parent
	}
	return path, owner, kind
}

// publish sends an event to subscribers, keeps it for the lease's trace
// and writes it to the audit log. lease is empty for host events.
func (m *Manager) publish(typ string, lease api.LeaseID, data map[string]any) {
	o := m.obs
	path, owner, kind := m.scope(lease)
	e := o.bus.Publish(telemetry.Scoped{Event: api.Event{Type: typ, Lease: lease, Data: data}, Path: path, OwnerUID: owner})
	if lease != "" {
		o.mu.Lock()
		l := o.lease(lease)
		if len(l.events) < maxEvents {
			l.events = append(l.events, e.Event)
		}
		o.mu.Unlock()
	}
	if o.audit != nil {
		o.audit.Write(audit.Record{At: e.At, Type: audit.TypeEvent, Lease: lease, Path: path, Kind: kind, OwnerUID: owner, Event: &e.Event})
	}
	if lease != "" {
		m.notifyInbox(typ, lease)
	}
}

// auditLease writes a record about a lease.
func (m *Manager) auditLease(typ string, ls api.Lease, fill func(*audit.Record)) {
	if m.obs.audit == nil {
		return
	}
	path, owner, _ := m.scope(ls.ID)
	if len(path) == 0 { // already forgotten by the ledger
		path, owner = []api.LeaseID{ls.ID}, ls.OwnerUID
	}
	r := audit.Record{Type: typ, Lease: ls.ID, Path: path, Kind: ls.Kind, OwnerUID: owner, State: ls.State}
	if fill != nil {
		fill(&r)
	}
	m.obs.audit.Write(r)
}

// ended records the end of a lease everywhere it is counted.
func (m *Manager) ended(ls api.Lease, term api.Termination) {
	o := m.obs
	o.metrics.Termination(term.Code, ls.Kind, term.Peak)
	o.suggest.Add(ls.Kind, term.Peak, term.Code)
	o.mu.Lock()
	o.lease(ls.ID).term = &term
	o.ended = append(o.ended, ls.ID)
	for len(o.ended) > keptTraces {
		delete(o.leases, o.ended[0])
		o.ended = o.ended[1:]
	}
	o.mu.Unlock()
	var data map[string]any
	if b, err := json.Marshal(term); err == nil {
		_ = json.Unmarshal(b, &data)
	}
	m.publish(telemetry.EventTerminated, ls.ID, data)
	m.auditLease(audit.TypeTerminate, ls, func(r *audit.Record) { r.State, r.Termination, r.Envelope = api.StateReaped, &term, &ls.Envelope })
}

// tickLoop runs once per tick and does everything that is periodic:
// sampling, the warnings derived from samples, the two limits the kernel
// does not enforce, capacity tracking and the safety net.
func (m *Manager) tickLoop(ctx context.Context) {
	tick := time.NewTicker(m.cfg.Tick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if m.abandoned.Load() {
			return
		}
		usage := m.sampleAll(ctx)
		m.trackCapacity(usage)
		m.checkIdle()
	}
}

// limitLoop is the memory limit of a backend that can only monitor
// memory: it checks every lease more often than the main tick, because a
// lease overshoots its limit by what it allocates between two checks.
// Everything else (time lines, warnings, idleness) stays on the tick.
func (m *Manager) limitLoop(ctx context.Context) {
	tick := time.NewTicker(m.cfg.LimitCheck)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if m.abandoned.Load() {
			return
		}
		m.mu.Lock()
		live := make([]liveLease, 0, len(m.rt))
		for id, rt := range m.rt {
			live = append(live, liveLease{id: id, group: rt.group})
		}
		m.mu.Unlock()
		for _, r := range live {
			ls, ok := m.led.Get(r.id)
			if !ok || ls.State != api.StateRunning || ls.Envelope.Memory <= 0 {
				continue
			}
			if u, err := m.be.Sample(ctx, r.group); err == nil && u.Memory > ls.Envelope.Memory {
				go m.reap(r.id, api.CodeOOMLeaseLimit, map[string]any{"detected_by": "sampling", "usage": u.Memory})
			}
		}
	}
}

type liveLease struct {
	id      api.LeaseID
	group   backend.ACGroup
	started time.Time
	running bool
	tty     *ttyRelay
}

// sampleAll samples every live lease and acts on what the samples show.
// It returns each lease's memory usage for the capacity step.
func (m *Manager) sampleAll(ctx context.Context) map[api.LeaseID]int64 {
	var live []liveLease
	m.mu.Lock()
	for id, rt := range m.rt {
		live = append(live, liveLease{id, rt.group, rt.started, rt.proc != nil, rt.tty})
	}
	m.mu.Unlock()
	meter, _ := m.be.(backend.OutputMeter)
	monitorOnly := m.be.Capabilities().Memory == api.LevelMonitorOnly
	usage := make(map[api.LeaseID]int64, len(live))
	for _, r := range live {
		ls, ok := m.led.Get(r.id)
		if !ok || (ls.State != api.StateRunning && ls.State != api.StateAdmitted) {
			continue
		}
		u, err := m.be.Sample(ctx, r.group)
		if err != nil {
			continue
		}
		usage[r.id] = u.Memory
		m.observeSample(ctx, ls, u)
		// A backend that can only monitor memory has no kernel limit to
		// rely on: the daemon is the limit, one sample late.
		if monitorOnly && u.Memory > ls.Envelope.Memory {
			go m.reap(r.id, api.CodeOOMLeaseLimit, map[string]any{"detected_by": "sampling", "usage": u.Memory})
			continue
		}
		if !r.running {
			continue
		}
		if wall := ls.Envelope.Wall; wall > 0 {
			if elapsed := time.Since(r.started); elapsed >= time.Duration(wall)*time.Second {
				go m.reap(r.id, api.CodeWallExceeded, map[string]any{"wall_limit_seconds": wall, "elapsed_seconds": elapsed.Seconds()})
				continue
			}
		}
		if limit := ls.Envelope.Output; limit > 0 && r.tty != nil {
			// A lead writes to its terminal, not to files: what was relayed
			// is what counts.
			if n := r.tty.bytes.Load(); n > limit {
				go m.reap(r.id, api.CodeOutputExceeded, map[string]any{"output_limit_bytes": limit, "output_bytes": n})
			}
		} else if limit > 0 && meter != nil {
			if n, err := meter.OutputBytes(ctx, r.group); err == nil && n > limit {
				go m.reap(r.id, api.CodeOutputExceeded, map[string]any{"output_limit_bytes": limit, "output_bytes": n})
			}
		}
	}
	return usage
}

// observeSample stores a sample and raises NEAR_LIMIT when usage reaches
// the warning share of the hard limit. The warning is sent once and
// re-arms when usage has fallen back, so a lease hovering at the line does
// not produce a stream of them.
func (m *Manager) observeSample(ctx context.Context, ls api.Lease, u api.Usage) {
	o := m.obs
	limit := ls.Envelope.Memory
	o.mu.Lock()
	l := o.lease(ls.ID)
	l.ring.Add(u)
	l.ticks++
	toAudit := l.ticks%sampleEvery == 0
	rate := l.ring.Rate()
	warn := false
	switch {
	case !l.near && float64(u.Memory) >= m.cfg.NearLimit*float64(limit):
		l.near, warn = true, true
	case l.near && float64(u.Memory) < rearmFraction*float64(limit):
		l.near = false
	}
	// Keep a list of the largest processes while the lease is close to its
	// limit, or growing fast enough to reach it within two samples: the
	// kernel's group kill leaves nothing to list afterwards.
	watch := l.near || float64(u.Memory) >= topProcsFrom*float64(limit) ||
		limit > 0 && float64(u.Memory)+rate*2*m.cfg.Tick.Seconds() >= float64(limit)
	o.mu.Unlock()
	if lister, ok := m.be.(backend.ProcLister); ok && watch {
		if rt := m.runtime(ls.ID); rt != nil {
			if top, err := lister.TopProcs(ctx, rt.group, topProcs); err == nil && len(top) > 0 {
				o.mu.Lock()
				l.top, l.topAt = top, time.Now()
				o.mu.Unlock()
			}
		}
	}
	if toAudit {
		m.auditLease(audit.TypeSample, ls, func(r *audit.Record) { r.Usage = &u })
	}
	if !warn {
		return
	}
	m.publish(telemetry.EventNearLimit, ls.ID, map[string]any{
		"usage": u.Memory, "limit": limit, "mem_basis": u.MemBasis, "rate_bytes_per_second": rate,
	})
	// The one thing subactld does on a warning, and only because the
	// planner declared it in advance: grow within what the parent has free.
	if g := ls.OnNearLimit; g != nil && g.GrowTo > limit {
		if _, err := m.applyResize(ctx, ls, api.EnvelopePatch{Memory: &g.GrowTo}, "on_near_limit"); err == nil {
			o.mu.Lock()
			o.lease(ls.ID).near = false // armed again, against the new limit
			o.mu.Unlock()
		}
	}
}

// trackCapacity recomputes the effective capacity from a fresh host
// reading and runs the safety net.
func (m *Manager) trackCapacity(usage map[api.LeaseID]int64) {
	if m.cfg.Host == nil {
		return
	}
	o := m.obs
	h, err := m.cfg.Host.Read()
	if err != nil {
		m.log.Warn("cannot read host memory", "err", err)
		return
	}
	var managed int64
	for _, id := range m.led.Leads() {
		managed += usage[id] // a lead's usage includes its whole tree
	}
	external := capacity.External(h, managed)
	shrunk := m.led.SetEffectiveCapacity(capacity.Effective(m.cfg.Capacity, h, m.cfg.Reserve, external))
	o.mu.Lock()
	was := o.shrunk
	o.host, o.external, o.shrunk = h, external, shrunk
	o.mu.Unlock()
	if shrunk && !was {
		// Allocated is not used: nothing is terminated for this. New
		// leases and raises are refused until it recovers.
		eff, alloc, _ := m.led.Totals()
		m.publish(telemetry.EventCapacityShrunk, "", map[string]any{
			"reason": "allocated_exceeds_capacity", "effective_capacity": eff, "allocated": alloc, "external": external,
		})
	}
	m.safetyNet(h, usage)
}

// safetyNet is the only action subactld takes on its own. Available
// memory must be below the reserve line for three readings in a row. Then
// planners get a window to release something themselves; after it, one
// lease at a time is terminated until memory is back above the line.
func (m *Manager) safetyNet(h capacity.Host, usage map[api.LeaseID]int64) {
	o := m.obs
	now := time.Now()
	if !o.net.Observe(h.Available, m.cfg.Reserve) {
		o.window = time.Time{}
		return
	}
	if o.window.IsZero() {
		o.window = now.Add(m.cfg.SafetyWindow)
		m.publish(telemetry.EventCapacityShrunk, "", map[string]any{
			"reason": "below_reserve", "available": h.Available, "reserve": m.cfg.Reserve,
			"deadline": o.window, "then": "leases are terminated one at a time (503) until memory recovers",
		})
		return
	}
	o.mu.Lock()
	busy := o.killing
	o.mu.Unlock()
	if now.Before(o.window) || busy {
		return
	}
	var nodes []capacity.Node
	for _, lead := range m.led.Leads() {
		for _, ls := range m.led.Subtree(lead) {
			if ls.State != api.StateRunning && ls.State != api.StateAdmitted {
				continue
			}
			n := capacity.Node{ID: ls.ID, Parent: ls.Parent, Lead: ls.Lead, Usage: usage[ls.ID], KillOrder: ls.KillOrder, Leaf: true}
			for _, c := range ls.Children {
				if cl, ok := m.led.Get(c); ok && (cl.State == api.StateRunning || cl.State == api.StateAdmitted) {
					n.Leaf = false
					break
				}
			}
			nodes = append(nodes, n)
		}
	}
	victim, ok := capacity.Victim(nodes)
	if !ok {
		return // nothing of ours to release; the pressure is external
	}
	o.mu.Lock()
	o.killing = true
	o.mu.Unlock()
	o.metrics.SafetyNet()
	// Start counting afresh: the next lease goes only after three new
	// readings below the line, taken once this one has released its memory.
	o.net = capacity.Net{Need: o.net.Need}
	go func() {
		m.reap(victim, api.CodeSafetyNet, map[string]any{"available": h.Available, "reserve": m.cfg.Reserve, "usage": usage[victim]})
		o.mu.Lock()
		o.killing = false
		o.mu.Unlock()
	}()
}

// Events subscribes the caller to the events it may see. The returned
// function ends the subscription.
func (m *Manager) Events(c Caller) (<-chan api.Event, func()) {
	sub := m.obs.bus.Subscribe(256)
	out := make(chan api.Event, 16)
	stop := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case <-stop:
				return
			case e := <-sub.C:
				if !e.Visible(c.Lease, c.UID, c.Admin) {
					continue
				}
				select {
				case out <- e.Event:
				case <-stop:
					return
				}
			}
		}
	}()
	var once sync.Once
	return out, func() {
		once.Do(func() {
			m.obs.bus.Unsubscribe(sub)
			close(stop)
		})
	}
}

// Trace returns the timeline of a lease. A lease the daemon still
// remembers is answered from memory at one-second resolution; an older
// one is rebuilt from the audit log, at the log's five seconds.
func (m *Manager) Trace(c Caller, id api.LeaseID) (api.Trace, error) {
	if id == api.Self && c.Lease != "" {
		id = c.Lease
	}
	o := m.obs
	if _, known := m.led.Get(id); known {
		if _, err := m.resolve(c, id); err != nil {
			return api.Trace{}, err
		}
	}
	o.mu.Lock()
	l, inMemory := o.leases[id]
	var tr api.Trace
	if inMemory {
		tr = api.Trace{Lease: id, Samples: l.ring.Samples(), Events: append([]api.Event{}, l.events...), Termination: l.term}
	}
	o.mu.Unlock()
	if inMemory {
		if _, known := m.led.Get(id); !known {
			// Remembered here but dropped by the ledger: check the caller
			// against the scope stored with its events in the audit log.
			if err := m.traceAllowed(c, id); err != nil {
				return api.Trace{}, err
			}
		}
		tr.Spikes = spikes(tr.Samples, tr.Events)
		return tr, nil
	}
	tr, err := m.traceFromAudit(c, id)
	if err == nil {
		tr.Spikes = spikes(tr.Samples, tr.Events)
	}
	return tr, err
}

// spikes attributes memory growth to tool calls. Each progress beacon that
// names a tool opens an interval that the next beacon closes; the growth
// of an interval is its highest sample minus the memory when it opened.
// Intervals that grew by at least api.SpikeMin are returned, largest
// first. Samples come once a second, so a call shorter than that may have
// its growth seen in the next interval.
func spikes(samples []api.Usage, events []api.Event) []api.Spike {
	type beacon struct {
		at   time.Time
		tool string
		seq  int64
	}
	var beacons []beacon
	for _, e := range events {
		if e.Type != eventBeacon {
			continue
		}
		b := beacon{at: e.At}
		b.tool, _ = e.Data["tool"].(string)
		switch v := e.Data["seq"].(type) {
		case int64:
			b.seq = v
		case float64: // read back from JSON
			b.seq = int64(v)
		}
		beacons = append(beacons, b)
	}
	sort.SliceStable(beacons, func(i, j int) bool { return beacons[i].at.Before(beacons[j].at) })
	var out []api.Spike
	for i, b := range beacons {
		if b.tool == "" {
			continue // a beacon of state alone opens no tool call
		}
		var until time.Time
		if i+1 < len(beacons) {
			until = beacons[i+1].at
		}
		before, peak, seen := int64(-1), int64(0), false
		for _, s := range samples {
			switch {
			case !s.SampledAt.After(b.at):
				before = s.Memory // the last sample before the call
			case until.IsZero() || !s.SampledAt.After(until):
				peak, seen = max(peak, s.Memory), true
			}
		}
		if !seen {
			continue
		}
		if before < 0 {
			before = 0
		}
		if g := peak - before; g >= api.SpikeMin {
			out = append(out, api.Spike{Tool: b.tool, Seq: b.seq, From: b.at, Until: until, Before: before, Peak: peak, Growth: g})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Growth > out[j].Growth })
	return out
}

// TraceTree is Trace with the timelines of the whole subtree as children,
// ended leases included: live ones are found in the ledger, recently ended
// ones in memory, older ones in the audit log.
func (m *Manager) TraceTree(c Caller, id api.LeaseID) (api.Trace, error) {
	if id == api.Self && c.Lease != "" {
		id = c.Lease
	}
	root, err := m.Trace(c, id)
	if err != nil {
		return api.Trace{}, err
	}
	parent := map[api.LeaseID]api.LeaseID{}
	if m.cfg.AuditDir != "" {
		if p, err := audit.Parents(m.cfg.AuditDir); err == nil {
			parent = p
		}
	}
	m.obs.mu.Lock()
	for lid, l := range m.obs.leases {
		if l.term != nil && l.term.Parent != "" {
			parent[lid] = l.term.Parent
		}
	}
	m.obs.mu.Unlock()
	for _, ls := range m.led.Subtree(id) {
		if ls.Parent != "" {
			parent[ls.ID] = ls.Parent
		}
	}
	children := map[api.LeaseID][]api.LeaseID{}
	for child, p := range parent {
		children[p] = append(children[p], child)
	}
	var build func(tr *api.Trace, depth int)
	build = func(tr *api.Trace, depth int) {
		kids := children[tr.Lease]
		sort.Slice(kids, func(i, j int) bool { return kids[i] < kids[j] })
		for _, k := range kids {
			if depth > 64 {
				return // a cycle would be a bug elsewhere; never loop for it
			}
			ct, err := m.Trace(c, k)
			if err != nil {
				continue // forgotten everywhere, or not the caller's to see
			}
			build(&ct, depth+1)
			tr.Children = append(tr.Children, ct)
		}
	}
	build(&root, 0)
	return root, nil
}

func (m *Manager) auditRecords(id api.LeaseID) []audit.Record {
	if m.cfg.AuditDir == "" {
		return nil
	}
	recs, err := audit.Read(m.cfg.AuditDir, id)
	if err != nil {
		m.log.Warn("cannot read the audit log", "err", err)
	}
	return recs
}

func allowedByRecord(c Caller, r audit.Record) bool {
	return telemetry.Scoped{Path: r.Path, OwnerUID: r.OwnerUID}.Visible(c.Lease, c.UID, c.Admin)
}

func (m *Manager) traceAllowed(c Caller, id api.LeaseID) error {
	for _, r := range m.auditRecords(id) {
		if len(r.Path) > 0 {
			if allowedByRecord(c, r) {
				return nil
			}
			break
		}
	}
	if c.Admin {
		return nil
	}
	return apiErr(http.StatusForbidden, api.ErrOutOfSubtree, "lease %s is outside what the caller may see", id)
}

func (m *Manager) traceFromAudit(c Caller, id api.LeaseID) (api.Trace, error) {
	recs := m.auditRecords(id)
	if len(recs) == 0 {
		return api.Trace{}, apiErr(http.StatusNotFound, api.ErrNotFound, "no lease %s, in memory or in the audit log", id)
	}
	if len(recs[0].Path) == 0 || !allowedByRecord(c, recs[0]) {
		return api.Trace{}, apiErr(http.StatusForbidden, api.ErrOutOfSubtree, "lease %s is outside what the caller may see", id)
	}
	tr := api.Trace{Lease: id, Samples: []api.Usage{}, Events: []api.Event{}}
	for _, r := range recs {
		switch {
		case r.Type == audit.TypeSample && r.Usage != nil:
			tr.Samples = append(tr.Samples, *r.Usage)
		case r.Type == audit.TypeEvent && r.Event != nil:
			tr.Events = append(tr.Events, *r.Event)
		case r.Type == audit.TypeTerminate:
			tr.Termination = r.Termination
		}
	}
	return tr, nil
}

// MetricsText renders the Prometheus metrics.
func (m *Manager) MetricsText() string {
	o := m.obs
	eff, alloc, shrunk := m.led.Totals()
	g := metrics.Gauges{
		EffectiveCapacity: eff, Allocated: alloc, Reserve: m.cfg.Reserve, CapacityShrunk: shrunk,
		LiveByDepth: map[int]int{}, DroppedEvents: o.bus.Dropped(),
	}
	o.mu.Lock()
	g.External = o.external
	o.mu.Unlock()
	if o.audit != nil {
		g.DroppedAudit = o.audit.Dropped()
	}
	for _, lead := range m.led.Leads() {
		for _, ls := range m.led.Subtree(lead) {
			if ls.State != api.StateReaped {
				g.LiveByDepth[ls.Depth]++
			}
		}
	}
	var b strings.Builder
	o.metrics.Write(&b, g)
	return b.String()
}
