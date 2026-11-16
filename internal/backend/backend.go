// Package backend defines the enforcement backend: the only place where
// platform mechanisms (cgroup, Job Object, kqueue) may appear. The core
// speaks only in the abstract terms of the design: acgroup, envelope,
// membership, kill_tree.
package backend

import (
	"context"
	"errors"
	"syscall"

	"github.com/zydo/subactl/api"
)

// ErrNotMember is returned by Identify when the pid is in no acgroup, which
// makes the caller an operator.
var ErrNotMember = errors.New("pid is not a member of any acgroup")

// ACGroupRef names an acgroup by the lease it belongs to. The zero value
// means "no parent", i.e. a top-level acgroup.
type ACGroupRef struct {
	Lease api.LeaseID
}

// ACGroup is a handle to a created acgroup.
type ACGroup interface {
	Ref() ACGroupRef
}

// Cmd describes the main process of a lease.
type Cmd struct {
	Argv []string
	Env  []string
	Dir  string
	UID  int
	TTY  bool
}

// HolderWatcher is implemented by backends that can tell exactly when a
// process outside any acgroup dies. The daemon uses it to reap a lead when
// its holder, the "subactl run" process, goes away.
type HolderWatcher interface {
	// WatchHolder returns a channel that is closed when pid exits. The
	// watch ends, without closing the channel, when ctx is cancelled.
	WatchHolder(ctx context.Context, pid int) (<-chan struct{}, error)
}

// Escape is a process found outside every acgroup that still carries a
// lease's marker in its environment.
type Escape struct {
	Pid   int
	Lease api.LeaseID
}

// EscapeScanner is implemented by backends that can look for such
// processes. It is a heuristic for accidental leaks, not a security
// boundary: clearing the environment defeats it.
type EscapeScanner interface {
	ScanEscapes(ctx context.Context) ([]Escape, error)
}

// ExitStatus is how a main process ended. Signal is zero for a normal exit.
type ExitStatus struct {
	Code   int
	Signal int
}

// Proc is the main process of a lease. The daemon is its parent, so Wait
// yields the exit status directly.
type Proc interface {
	Pid() int
	Wait() ExitStatus
	Signal(sig syscall.Signal) error
}

// KillReport is the outcome of KillTree. Remaining must be zero for the
// acgroup to count as reaped.
type KillReport struct {
	Killed    int
	Remaining int
}

// Backend creates acgroups, places processes in them before exec, samples
// them, kills them as a whole and destroys them. It also answers two
// queries: which acgroup a pid is in, and which acgroups exist.
type Backend interface {
	Name() string
	MemBasis() api.MemBasis
	Capabilities() api.Capabilities
	Create(ctx context.Context, lease api.LeaseID, parent ACGroupRef, env api.Envelope) (ACGroup, error)
	Spawn(ctx context.Context, g ACGroup, cmd Cmd) (Proc, error)
	Sample(ctx context.Context, g ACGroup) (api.Usage, error)
	SetLimits(ctx context.Context, g ACGroup, env api.Envelope) error
	KillTree(ctx context.Context, g ACGroup) (KillReport, error)
	Destroy(ctx context.Context, g ACGroup) error
	Identify(ctx context.Context, pid int) (ACGroupRef, error)
	List(ctx context.Context) ([]ACGroupRef, error)
}
