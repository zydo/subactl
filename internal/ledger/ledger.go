// Package ledger is the in-memory quota ledger and lease tree.
//
// It is pure accounting: it starts nothing and kills nothing. It answers
// each request with approval or with a rejection and a reason, and it never
// queues or chooses on the planner's behalf.
//
// The invariant it protects (I-B): the memory hard limits of all live
// top-level leases never sum to more than the effective capacity at the
// moment a reservation is made, and inside every lease the node's own
// reserve plus its live children's limits never exceed its own limit.
package ledger

import (
	"fmt"
	"sync"
	"time"

	"github.com/zydo/subactl/api"
)

const (
	// MinHeadroom is the floor of the margin kept above current usage when
	// a limit is lowered, so that a resize cannot itself kill the lease.
	MinHeadroom = 64 << 20
	// keptReaped bounds how many reaped children a live parent keeps for
	// queries, and how many finished lead trees the ledger keeps.
	keptReaped = 1024
)

// Rejection is a refusal with one of the reasons defined by the contract.
type Rejection struct {
	Reason  string
	Message string
}

func (r *Rejection) Error() string { return r.Reason + ": " + r.Message }

func reject(reason, format string, a ...any) *Rejection {
	return &Rejection{Reason: reason, Message: fmt.Sprintf(format, a...)}
}

// Params describes a lease to acquire.
type Params struct {
	ID          api.LeaseID
	Parent      api.LeaseID // empty for a lead
	Kind        string
	Envelope    api.Envelope
	OwnerUID    int // operator that started the tree
	UID         int // uid the agent runs as
	KillOrder   *int
	OnNearLimit *api.OnNearLimit
	DryRun      bool
}

type node struct {
	id          api.LeaseID
	parent      *node
	lead        *node
	kind        string
	depth       int
	state       api.State
	env         api.Envelope
	ownerUID    int
	uid         int
	killOrder   *int
	onNearLimit *api.OnNearLimit
	created     time.Time
	children    []*node // in creation order
	code        int     // set when reaping begins; first cause wins
	term        *api.Termination
}

func (n *node) live() bool { return n.state != api.StateReaped }

// childAlloc is the memory reserved by n's live children.
func (n *node) childAlloc(except *node) int64 {
	var sum int64
	for _, c := range n.children {
		if c != except && c.live() {
			sum += c.env.Memory
		}
	}
	return sum
}

// Ledger is safe for concurrent use.
type Ledger struct {
	mu        sync.Mutex
	effective int64
	leases    map[api.LeaseID]*node
	leads     []*node // in creation order, finished ones included
	now       func() time.Time
}

// New returns a ledger with the given effective capacity in bytes.
func New(effective int64) *Ledger {
	return &Ledger{effective: effective, leases: map[api.LeaseID]*node{}, now: time.Now}
}

func (l *Ledger) allocatedLocked(except *node) int64 {
	var sum int64
	for _, n := range l.leads {
		if n != except && n.live() {
			sum += n.env.Memory
		}
	}
	return sum
}

func (l *Ledger) shrunkLocked() bool { return l.allocatedLocked(nil) > l.effective }

// SetEffectiveCapacity updates the capacity. It never revokes anything:
// if allocation now exceeds capacity the ledger is "shrunk" and refuses new
// reservations and raises until it recovers.
func (l *Ledger) SetEffectiveCapacity(n int64) (shrunk bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.effective = n
	return l.shrunkLocked()
}

// Totals reports the effective capacity, the memory allocated to live
// top-level leases, and whether the ledger is shrunk.
func (l *Ledger) Totals() (effective, allocated int64, shrunk bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.effective, l.allocatedLocked(nil), l.shrunkLocked()
}

// DefaultLeadMemory is the envelope a lead gets when it names none:
// min(effective × 50%, remaining). It is never "all that is left", or the
// first lead would take the whole machine.
func (l *Ledger) DefaultLeadMemory() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return max(0, min(l.effective/2, l.effective-l.allocatedLocked(nil)))
}

// Acquire is the atomic check-and-reserve. On success the lease is admitted.
func (l *Ledger) Acquire(p Params) (api.Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, dup := l.leases[p.ID]; dup || p.ID == "" {
		return api.Lease{}, fmt.Errorf("ledger: bad or duplicate lease id %q", p.ID)
	}
	e := p.Envelope
	if e.Memory <= 0 || e.MemorySelf < 0 || e.MemorySelf > e.Memory {
		return api.Lease{}, fmt.Errorf("ledger: invalid envelope: memory %d, memory_self %d", e.Memory, e.MemorySelf)
	}
	n := &node{
		id: p.ID, kind: p.Kind, state: api.StateAdmitted, env: e,
		ownerUID: p.OwnerUID, uid: p.UID, killOrder: p.KillOrder, onNearLimit: p.OnNearLimit,
		created: l.now(),
	}
	if p.Parent == "" {
		if l.shrunkLocked() {
			return api.Lease{}, reject(api.ReasonCapacityShrunk, "allocated memory exceeds the effective capacity; no new leases until it recovers")
		}
		if alloc := l.allocatedLocked(nil); alloc+e.Memory > l.effective {
			return api.Lease{}, reject(api.ReasonMemBudget, "requested %s, but only %s of %s effective capacity is unallocated",
				api.FormatBytes(e.Memory), api.FormatBytes(max(0, l.effective-alloc)), api.FormatBytes(l.effective))
		}
		n.lead = n
	} else {
		parent, ok := l.leases[p.Parent]
		if !ok || parent.state != api.StateRunning {
			return api.Lease{}, reject(api.ReasonParentNotRunning, "parent %s is not running", p.Parent)
		}
		if l.shrunkLocked() {
			return api.Lease{}, reject(api.ReasonCapacityShrunk, "allocated memory exceeds the effective capacity; no new leases until it recovers")
		}
		if used := parent.childAlloc(nil) + parent.env.MemorySelf; used+e.Memory > parent.env.Memory {
			return api.Lease{}, reject(api.ReasonParentBudget, "requested %s, but parent %s has %s left (limit %s, memory_self %s, children %s)",
				api.FormatBytes(e.Memory), parent.id, api.FormatBytes(max(0, parent.env.Memory-used)),
				api.FormatBytes(parent.env.Memory), api.FormatBytes(parent.env.MemorySelf), api.FormatBytes(parent.childAlloc(nil)))
		}
		n.parent, n.lead, n.depth = parent, parent.lead, parent.depth+1
		n.ownerUID, n.uid = parent.ownerUID, parent.uid
	}
	if p.DryRun {
		return l.snapshot(n), nil
	}
	l.leases[n.id] = n
	if n.parent == nil {
		l.leads = append(l.leads, n)
	} else {
		n.parent.children = append(n.parent.children, n)
	}
	return l.snapshot(n), nil
}

// Abort removes a lease that was admitted but could not be created by the
// backend. It releases the reservation.
func (l *Ledger) Abort(id api.LeaseID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.leases[id]
	if !ok || n.state != api.StateAdmitted || len(n.children) > 0 {
		return
	}
	l.dropLocked(n)
}

// Running moves an admitted lease to running.
func (l *Ledger) Running(id api.LeaseID) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.leases[id]
	if !ok {
		return fmt.Errorf("ledger: unknown lease %s", id)
	}
	if n.state != api.StateAdmitted {
		return fmt.Errorf("ledger: lease %s is %s, not admitted", id, n.state)
	}
	n.state = api.StateRunning
	return nil
}

// Marked is a lease that BeginReap moved to reaping, with its cause.
type Marked struct {
	ID   api.LeaseID
	Code int
}

// BeginReap moves the lease and every descendant that is not already
// reaping or reaped to reaping. The lease takes the given code; its
// descendants take 502 PARENT_DIED. A lease already reaping keeps its first
// code, which is what makes "exactly one termination code" hold. The result
// lists the leases moved by this call, children before parents.
func (l *Ledger) BeginReap(id api.LeaseID, code int) []Marked {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.leases[id]
	if !ok {
		return nil
	}
	var out []Marked
	var walk func(n *node, code int)
	walk = func(n *node, code int) {
		for _, c := range n.children {
			walk(c, api.CodeParentDied)
		}
		if n.state == api.StateAdmitted || n.state == api.StateRunning {
			n.state, n.code = api.StateReaping, code
			out = append(out, Marked{n.id, code})
		}
	}
	if n.state == api.StateAdmitted || n.state == api.StateRunning {
		walk(n, code)
	}
	return out
}

// Finish moves a reaping lease to reaped, records its termination and
// releases its reservation. Every child must already be reaped.
func (l *Ledger) Finish(id api.LeaseID, term api.Termination) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.leases[id]
	if !ok {
		return fmt.Errorf("ledger: unknown lease %s", id)
	}
	if n.state != api.StateReaping {
		return fmt.Errorf("ledger: lease %s is %s, not reaping", id, n.state)
	}
	for _, c := range n.children {
		if c.live() {
			return fmt.Errorf("ledger: lease %s still has live child %s", id, c.id)
		}
	}
	n.state, n.term = api.StateReaped, &term
	// Bound what is kept for queries.
	if n.parent != nil {
		l.trimLocked(&n.parent.children)
	} else {
		l.trimLocked(&l.leads)
	}
	return nil
}

// trimLocked drops the oldest reaped entries beyond keptReaped.
func (l *Ledger) trimLocked(list *[]*node) {
	reaped := 0
	for _, n := range *list {
		if !n.live() {
			reaped++
		}
	}
	for i := 0; reaped > keptReaped && i < len(*list); {
		if n := (*list)[i]; !n.live() {
			l.dropLocked(n) // removes n from *list
			reaped--
			continue
		}
		i++
	}
}

// dropLocked forgets n and its whole subtree.
func (l *Ledger) dropLocked(n *node) {
	var forget func(n *node)
	forget = func(n *node) {
		for _, c := range n.children {
			forget(c)
		}
		delete(l.leases, n.id)
	}
	forget(n)
	list := &l.leads
	if n.parent != nil {
		list = &n.parent.children
	}
	for i, c := range *list {
		if c == n {
			*list = append((*list)[:i], (*list)[i+1:]...)
			break
		}
	}
}

// Code returns the cause recorded when reaping began, or 0.
func (l *Ledger) Code(id api.LeaseID) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n, ok := l.leases[id]; ok {
		return n.code
	}
	return 0
}

// Resize applies a patch to a live lease. usage is the lease's current
// memory usage in bytes, needed to refuse a limit that would kill it.
func (l *Ledger) Resize(id api.LeaseID, p api.EnvelopePatch, usage int64) (api.Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.leases[id]
	if !ok || (n.state != api.StateAdmitted && n.state != api.StateRunning) {
		return api.Lease{}, fmt.Errorf("ledger: lease %s is not live", id)
	}
	e := n.env
	if p.Memory != nil {
		e.Memory = *p.Memory
	}
	if p.MemorySelf != nil {
		e.MemorySelf = *p.MemorySelf
	}
	if p.CPU != nil {
		e.CPU = *p.CPU
	}
	if p.Pids != nil {
		e.Pids = *p.Pids
	}
	if p.Wall != nil {
		e.Wall = *p.Wall
	}
	if p.Output != nil {
		e.Output = *p.Output
	}
	if e.Memory <= 0 || e.MemorySelf < 0 || e.MemorySelf > e.Memory {
		return api.Lease{}, fmt.Errorf("ledger: invalid envelope: memory %d, memory_self %d", e.Memory, e.MemorySelf)
	}
	if e.Memory > n.env.Memory {
		if l.shrunkLocked() {
			return api.Lease{}, reject(api.ReasonCapacityShrunk, "allocated memory exceeds the effective capacity; no raises until it recovers")
		}
		if n.parent == nil {
			if others := l.allocatedLocked(n); others+e.Memory > l.effective {
				return api.Lease{}, reject(api.ReasonMemBudget, "cannot raise to %s: only %s is unallocated beyond the current %s",
					api.FormatBytes(e.Memory), api.FormatBytes(max(0, l.effective-others-n.env.Memory)), api.FormatBytes(n.env.Memory))
			}
		} else if used := n.parent.childAlloc(n) + n.parent.env.MemorySelf; used+e.Memory > n.parent.env.Memory {
			return api.Lease{}, reject(api.ReasonParentBudget, "cannot raise to %s: parent %s can give at most %s",
				api.FormatBytes(e.Memory), n.parent.id, api.FormatBytes(max(0, n.parent.env.Memory-used)))
		}
	}
	if e.Memory < n.env.Memory {
		if floor := usage + max(usage/10, MinHeadroom); e.Memory < floor {
			return api.Lease{}, reject(api.ReasonBelowUsage, "cannot lower to %s: usage is %s, so the floor is %s",
				api.FormatBytes(e.Memory), api.FormatBytes(usage), api.FormatBytes(floor))
		}
	}
	// The node's own reserve plus what its children hold must still fit.
	if need := e.MemorySelf + n.childAlloc(nil); need > e.Memory {
		return api.Lease{}, reject(api.ReasonBelowAllocated, "memory %s is less than memory_self %s plus %s held by child leases",
			api.FormatBytes(e.Memory), api.FormatBytes(e.MemorySelf), api.FormatBytes(n.childAlloc(nil)))
	}
	if !p.DryRun {
		n.env = e
	}
	out := l.snapshot(n)
	out.Envelope = e
	return out, nil
}

// Get returns a snapshot of one lease.
func (l *Ledger) Get(id api.LeaseID) (api.Lease, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.leases[id]
	if !ok {
		return api.Lease{}, false
	}
	return l.snapshot(n), true
}

// InSubtree reports whether id is root or one of its descendants.
func (l *Ledger) InSubtree(root, id api.LeaseID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for n := l.leases[id]; n != nil; n = n.parent {
		if n.id == root {
			return true
		}
	}
	return false
}

// Subtree returns the lease and its descendants in pre-order.
func (l *Ledger) Subtree(id api.LeaseID) []api.Lease {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.leases[id]
	if !ok {
		return nil
	}
	var out []api.Lease
	var walk func(n *node)
	walk = func(n *node) {
		out = append(out, l.snapshot(n))
		for _, c := range n.children {
			walk(c)
		}
	}
	walk(n)
	return out
}

// Leads returns the ids of all lead leases still kept, oldest first.
func (l *Ledger) Leads() []api.LeaseID {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]api.LeaseID, len(l.leads))
	for i, n := range l.leads {
		out[i] = n.id
	}
	return out
}

// Available reports the remaining budget of a lease's subtree.
func (l *Ledger) Available(id api.LeaseID) (api.Available, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.leases[id]
	if !ok {
		return api.Available{}, false
	}
	a := api.Available{
		Lease: n.id, MemoryLimit: n.env.Memory, MemorySelf: n.env.MemorySelf,
		MemoryAllocated: n.childAlloc(nil), HostEffectiveCapacity: l.effective,
		Suggest: map[string]api.Suggestion{},
	}
	a.MemoryFree = max(0, a.MemoryLimit-a.MemorySelf-a.MemoryAllocated)
	for _, c := range n.children {
		if c.live() {
			a.LiveChildren++
		}
	}
	return a, true
}

func (l *Ledger) snapshot(n *node) api.Lease {
	out := api.Lease{
		ID: n.id, Kind: n.kind, Depth: n.depth, State: n.state, Envelope: n.env,
		UID: n.uid, OwnerUID: n.ownerUID, CreatedAt: n.created,
		Children: []api.LeaseID{}, KillOrder: n.killOrder, OnNearLimit: n.onNearLimit, Termination: n.term,
	}
	if n.lead != nil {
		out.Lead = n.lead.id
	}
	if n.parent != nil {
		out.Parent = n.parent.id
	}
	for _, c := range n.children {
		out.Children = append(out.Children, c.id)
	}
	return out
}

// Check verifies the structural invariants and returns the first violation.
// It is meant for tests.
func (l *Ledger) Check() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := 0
	var walk func(n *node) error
	walk = func(n *node) error {
		seen++
		if l.leases[n.id] != n {
			return fmt.Errorf("%s: not indexed", n.id)
		}
		if n.live() {
			if used := n.env.MemorySelf + n.childAlloc(nil); used > n.env.Memory {
				return fmt.Errorf("%s: memory_self + children = %d exceeds limit %d", n.id, used, n.env.Memory)
			}
		}
		for _, c := range n.children {
			if c.parent != n || c.depth != n.depth+1 || c.lead != n.lead {
				return fmt.Errorf("%s: bad link to parent %s", c.id, n.id)
			}
			if c.live() && (n.state == api.StateReaped) {
				return fmt.Errorf("%s: live under reaped parent %s", c.id, n.id)
			}
			if (c.state == api.StateAdmitted || c.state == api.StateRunning) && n.state == api.StateReaping {
				return fmt.Errorf("%s: %s under reaping parent %s", c.id, c.state, n.id)
			}
			if err := walk(c); err != nil {
				return err
			}
		}
		return nil
	}
	for _, n := range l.leads {
		if n.parent != nil || n.depth != 0 || n.lead != n {
			return fmt.Errorf("%s: bad lead", n.id)
		}
		if err := walk(n); err != nil {
			return err
		}
	}
	if seen != len(l.leases) {
		return fmt.Errorf("index holds %d leases, tree holds %d", len(l.leases), seen)
	}
	return nil
}
