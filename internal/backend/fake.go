package backend

import (
	"context"
	"fmt"
	"sync"
	"syscall"
	"time"

	"github.com/zydo/subactl/api"
)

// Fake is an in-memory backend for testing the ledger, admission and the
// daemon's lifecycle logic. It starts no real processes and enforces
// nothing: tests script usage, exits and failures through its methods.
type Fake struct {
	mu      sync.Mutex
	caps    api.Capabilities
	groups  map[api.LeaseID]*fakeGroup
	nextPid int

	// CreateErr, when set, makes the next Create fail with it.
	CreateErr error
	// SpawnErr, when set, makes the next Spawn fail with it.
	SpawnErr error
}

type fakeGroup struct {
	id       api.LeaseID
	parent   api.LeaseID
	env      api.Envelope
	main     *FakeProc
	extra    int   // descendant processes besides main
	stuck    int   // processes that survive KillTree
	mem      int64 // own usage, children excluded
	ooms     int64 // times its own limit was hit
	output   int64 // bytes of stdout and stderr
	peak     int64
	children map[api.LeaseID]bool
}

func (g *fakeGroup) Ref() ACGroupRef { return ACGroupRef{Lease: g.id} }

// FakeProc is a simulated main process.
type FakeProc struct {
	f          *Fake
	pid        int
	done       chan struct{}
	status     ExitStatus
	exited     bool
	ignoreTERM bool
	Cmd        Cmd
}

// NewFake returns a fake backend that claims hard enforcement everywhere,
// so that tests of ordinary flows are not rejected. Use SetCapabilities to
// test weaker platforms.
func NewFake() *Fake {
	h := api.LevelHard
	return &Fake{
		caps: api.Capabilities{
			Membership: h, Memory: h, CPU: h, Pids: h, KillTree: h,
			ParentDeath: h, EscapePrevention: h, CallerIdentity: h,
		},
		groups:  map[api.LeaseID]*fakeGroup{},
		nextPid: 1000,
	}
}

func (f *Fake) Name() string           { return "fake" }
func (f *Fake) MemBasis() api.MemBasis { return api.BasisWorkingSet }

func (f *Fake) Capabilities() api.Capabilities {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.caps
}

// SetCapabilities replaces the reported capability matrix.
func (f *Fake) SetCapabilities(c api.Capabilities) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.caps = c
}

func (f *Fake) Create(_ context.Context, lease api.LeaseID, parent ACGroupRef, env api.Envelope) (ACGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.CreateErr; err != nil {
		f.CreateErr = nil
		return nil, err
	}
	if _, dup := f.groups[lease]; dup {
		return nil, fmt.Errorf("fake: acgroup %s already exists", lease)
	}
	if parent.Lease != "" {
		p, ok := f.groups[parent.Lease]
		if !ok {
			return nil, fmt.Errorf("fake: parent acgroup %s does not exist", parent.Lease)
		}
		p.children[lease] = true
	}
	g := &fakeGroup{id: lease, parent: parent.Lease, env: env, children: map[api.LeaseID]bool{}}
	f.groups[lease] = g
	return g, nil
}

func (f *Fake) group(g ACGroup) (*fakeGroup, error) {
	fg, ok := f.groups[g.Ref().Lease]
	if !ok {
		return nil, fmt.Errorf("fake: acgroup %s does not exist", g.Ref().Lease)
	}
	return fg, nil
}

func (f *Fake) Spawn(_ context.Context, g ACGroup, cmd Cmd) (Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.SpawnErr; err != nil {
		f.SpawnErr = nil
		return nil, err
	}
	fg, err := f.group(g)
	if err != nil {
		return nil, err
	}
	if fg.main != nil {
		return nil, fmt.Errorf("fake: acgroup %s already has a main process", fg.id)
	}
	f.nextPid++
	fg.main = &FakeProc{f: f, pid: f.nextPid, done: make(chan struct{}), Cmd: cmd}
	return fg.main, nil
}

// subtreeLocked sums memory and process counts over g and its descendants.
func (f *Fake) subtreeLocked(g *fakeGroup) (mem int64, pids int64) {
	mem = g.mem
	pids = int64(g.extra + g.stuck)
	if g.main != nil && !g.main.exited {
		pids++
	}
	for c := range g.children {
		m, p := f.subtreeLocked(f.groups[c])
		mem += m
		pids += p
	}
	return mem, pids
}

func (f *Fake) Sample(_ context.Context, g ACGroup) (api.Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fg, err := f.group(g)
	if err != nil {
		return api.Usage{}, err
	}
	mem, pids := f.subtreeLocked(fg)
	if mem > fg.peak {
		fg.peak = mem
	}
	return api.Usage{Memory: mem, MemoryPeak: fg.peak, MemBasis: f.MemBasis(), Pids: pids, OOMEvents: fg.ooms, SampledAt: time.Now()}, nil
}

func (f *Fake) SetLimits(_ context.Context, g ACGroup, env api.Envelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	fg, err := f.group(g)
	if err != nil {
		return err
	}
	fg.env = env
	return nil
}

func (f *Fake) killLocked(g *fakeGroup, rep *KillReport) {
	for c := range g.children {
		f.killLocked(f.groups[c], rep)
	}
	rep.Killed += g.extra
	g.extra = 0
	rep.Remaining += g.stuck
	if g.main != nil && !g.main.exited {
		g.main.exitLocked(ExitStatus{Signal: int(syscall.SIGKILL)})
		rep.Killed++
	}
}

func (f *Fake) KillTree(_ context.Context, g ACGroup) (KillReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fg, err := f.group(g)
	if err != nil {
		return KillReport{}, err
	}
	var rep KillReport
	f.killLocked(fg, &rep)
	return rep, nil
}

func (f *Fake) Destroy(_ context.Context, g ACGroup) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	fg, err := f.group(g)
	if err != nil {
		return err
	}
	if len(fg.children) > 0 {
		return fmt.Errorf("fake: acgroup %s still has child acgroups", fg.id)
	}
	if _, pids := f.subtreeLocked(fg); pids-int64(fg.stuck) > 0 {
		return fmt.Errorf("fake: acgroup %s still has processes", fg.id)
	}
	if p, ok := f.groups[fg.parent]; ok {
		delete(p.children, fg.id)
	}
	delete(f.groups, fg.id)
	return nil
}

func (f *Fake) Identify(_ context.Context, pid int) (ACGroupRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, g := range f.groups {
		if g.main != nil && g.main.pid == pid && !g.main.exited {
			return g.Ref(), nil
		}
	}
	return ACGroupRef{}, ErrNotMember
}

func (f *Fake) List(context.Context) ([]ACGroupRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ACGroupRef, 0, len(f.groups))
	for _, g := range f.groups {
		out = append(out, g.Ref())
	}
	return out, nil
}

func (p *FakeProc) Pid() int { return p.pid }

func (p *FakeProc) Wait() ExitStatus {
	<-p.done
	return p.status
}

// Signal delivers SIGKILL always and SIGTERM unless the process was told to
// ignore it. Other signals are accepted and ignored.
func (p *FakeProc) Signal(sig syscall.Signal) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	if p.exited {
		return fmt.Errorf("fake: process %d has exited", p.pid)
	}
	if sig == syscall.SIGKILL || (sig == syscall.SIGTERM && !p.ignoreTERM) {
		p.exitLocked(ExitStatus{Signal: int(sig)})
	}
	return nil
}

func (p *FakeProc) exitLocked(st ExitStatus) {
	if p.exited {
		return
	}
	p.exited = true
	p.status = st
	close(p.done)
}

// Test controls. Each panics on an unknown lease: that is a test bug.

func (f *Fake) must(lease api.LeaseID) *fakeGroup {
	g, ok := f.groups[lease]
	if !ok {
		panic(fmt.Sprintf("fake: no acgroup for lease %s", lease))
	}
	return g
}

// Exit makes the main process of lease end by itself with st.
func (f *Fake) Exit(lease api.LeaseID, st ExitStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.must(lease)
	if g.main == nil {
		panic(fmt.Sprintf("fake: lease %s has no main process", lease))
	}
	g.main.exitLocked(st)
}

// SetUsage sets the memory used by the lease's own processes.
func (f *Fake) SetUsage(lease api.LeaseID, mem int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.must(lease).mem = mem
}

// AddProcs adds n descendant processes that outlive the main process until
// the acgroup is killed.
func (f *Fake) AddProcs(lease api.LeaseID, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.must(lease).extra += n
}

// AddStuckProcs adds n processes that survive KillTree.
func (f *Fake) AddStuckProcs(lease api.LeaseID, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.must(lease).stuck += n
}

// IgnoreTERM makes the main process of lease ignore SIGTERM.
func (f *Fake) IgnoreTERM(lease api.LeaseID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.must(lease).main.ignoreTERM = true
}

// Limits returns the envelope last applied to the lease's acgroup.
func (f *Fake) Limits(lease api.LeaseID) api.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.must(lease).env
}

// Exists reports whether the lease still has an acgroup.
func (f *Fake) Exists(lease api.LeaseID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.groups[lease]
	return ok
}

// Preexisting plants an acgroup with n processes, as if left over from a
// previous daemon, for fail-closed startup tests.
func (f *Fake) Preexisting(lease api.LeaseID, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups[lease] = &fakeGroup{id: lease, extra: n, children: map[api.LeaseID]bool{}}
}

// Handle returns a handle for an acgroup known only by reference, such as
// one reported by List at startup. Every backend resolves handles by Ref.
func Handle(ref ACGroupRef) ACGroup { return handle(ref) }

type handle ACGroupRef

func (h handle) Ref() ACGroupRef { return ACGroupRef(h) }

// ExitIfAlive is Exit for tests that race exits against kills: it does
// nothing, and reports false, when the main process is already gone.
func (f *Fake) ExitIfAlive(lease api.LeaseID, st ExitStatus) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.groups[lease]
	if !ok || g.main == nil || g.main.exited {
		return false
	}
	g.main.exitLocked(st)
	return true
}

// OOM simulates the lease hitting its own memory limit: the kernel kills
// the whole acgroup, nested acgroups included, and records the event on
// the lease whose limit it was.
func (f *Fake) OOM(lease api.LeaseID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.must(lease)
	g.ooms++
	var rep KillReport
	f.killLocked(g, &rep)
}

// SetOutput sets how much the lease has written to stdout and stderr.
func (f *Fake) SetOutput(lease api.LeaseID, n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.must(lease).output = n
}

// OutputBytes implements OutputMeter.
func (f *Fake) OutputBytes(_ context.Context, g ACGroup) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fg, err := f.group(g)
	if err != nil {
		return 0, err
	}
	return fg.output, nil
}

// OwnUsage returns the memory of the lease's own processes, nested
// acgroups excluded, or zero if the acgroup is gone.
func (f *Fake) OwnUsage(lease api.LeaseID) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if g, ok := f.groups[lease]; ok {
		return g.mem
	}
	return 0
}
