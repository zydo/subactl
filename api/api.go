// Package api holds the wire types of the subactld HTTP/JSON API.
//
// The contract is openapi.yaml in this directory; these types mirror its
// schemas and a conformance test keeps the two in step.
package api

import (
	_ "embed"
	"time"
)

// OpenAPI is the contract, embedded so tests and tools read the same bytes.
//
//go:embed openapi.yaml
var OpenAPI []byte

// LeaseID identifies a lease, e.g. "ls_7f3a9c21".
type LeaseID string

// Self is an alias a node may use in place of its own lease id.
const Self LeaseID = "self"

// Level is an enforcement level reported by a backend for one capability.
type Level string

const (
	LevelHard        Level = "hard"
	LevelSoft        Level = "soft"
	LevelMonitorOnly Level = "monitor_only"
	LevelUnsupported Level = "unsupported"
)

func (l Level) rank() int {
	switch l {
	case LevelHard:
		return 3
	case LevelSoft:
		return 2
	case LevelMonitorOnly:
		return 1
	}
	return 0
}

// Valid reports whether l is one of the four defined levels.
func (l Level) Valid() bool {
	return l == LevelHard || l == LevelSoft || l == LevelMonitorOnly || l == LevelUnsupported
}

// Satisfies reports whether l is at least as strong as min.
func (l Level) Satisfies(min Level) bool { return l.rank() >= min.rank() }

// MemBasis says how a memory figure was measured. Figures with different
// bases must never be added or compared.
type MemBasis string

const (
	BasisWorkingSet    MemBasis = "working_set"
	BasisCommit        MemBasis = "commit"
	BasisPhysFootprint MemBasis = "phys_footprint"
)

// State is a lease state. The only path is admitted, running, reaping, reaped.
type State string

const (
	StateAdmitted State = "admitted"
	StateRunning  State = "running"
	StateReaping  State = "reaping"
	StateReaped   State = "reaped"
)

// Capabilities is the enforcement level a backend reaches for each capability.
type Capabilities struct {
	Membership       Level `json:"membership"`
	Memory           Level `json:"memory"`
	CPU              Level `json:"cpu"`
	Pids             Level `json:"pids"`
	KillTree         Level `json:"kill_tree"`
	ParentDeath      Level `json:"parent_death"`
	EscapePrevention Level `json:"escape_prevention"`
	CallerIdentity   Level `json:"caller_identity"`
}

// Host is the machine-wide view returned by GET /v1/host. Sizes are bytes.
type Host struct {
	Backend           string       `json:"backend"`
	MemBasis          MemBasis     `json:"mem_basis"`
	MemTotal          int64        `json:"mem_total"`
	Reserve           int64        `json:"reserve"`
	External          int64        `json:"external"`
	EffectiveCapacity int64        `json:"effective_capacity"`
	Allocated         int64        `json:"allocated"`
	CapacityShrunk    bool         `json:"capacity_shrunk"`
	Capabilities      Capabilities `json:"capabilities"`
}

// Envelope is the per-dimension ceiling of a lease. Memory figures are bytes,
// CPU is cores, Wall is seconds, Output is bytes. Zero means unlimited for
// every field except Memory.
type Envelope struct {
	Memory     int64   `json:"memory"`
	MemorySelf int64   `json:"memory_self"`
	CPU        float64 `json:"cpu"`
	Pids       int64   `json:"pids"`
	Wall       int64   `json:"wall"`
	Output     int64   `json:"output"`
}

// EnvelopePatch changes some dimensions of a running lease.
type EnvelopePatch struct {
	Memory     *int64   `json:"memory,omitempty"`
	MemorySelf *int64   `json:"memory_self,omitempty"`
	CPU        *float64 `json:"cpu,omitempty"`
	Pids       *int64   `json:"pids,omitempty"`
	Wall       *int64   `json:"wall,omitempty"`
	Output     *int64   `json:"output,omitempty"`
	DryRun     bool     `json:"dry_run,omitempty"`
}

// Require states the weakest enforcement level the caller accepts.
type Require struct {
	Memory   Level `json:"memory,omitempty"`
	CPU      Level `json:"cpu,omitempty"`
	Pids     Level `json:"pids,omitempty"`
	KillTree Level `json:"kill_tree,omitempty"`
}

// OnNearLimit is the declarative grow rule a planner may attach to a child.
type OnNearLimit struct {
	GrowTo int64 `json:"grow_to"`
}

// AcquireRequest is the body of POST /v1/leases.
type AcquireRequest struct {
	Parent      LeaseID      `json:"parent,omitempty"`
	Kind        string       `json:"kind,omitempty"`
	Envelope    Envelope     `json:"envelope"`
	Require     *Require     `json:"require,omitempty"`
	TTY         bool         `json:"tty,omitempty"`
	UID         *int         `json:"uid,omitempty"`
	KillOrder   *int         `json:"kill_order,omitempty"`
	OnNearLimit *OnNearLimit `json:"on_near_limit,omitempty"`
	DryRun      bool         `json:"dry_run,omitempty"`
}

// Usage is a measured sample of one acgroup, nested acgroups included.
type Usage struct {
	Memory     int64    `json:"memory"`
	MemoryPeak int64    `json:"memory_peak"`
	MemBasis   MemBasis `json:"mem_basis"`
	CPUSeconds float64  `json:"cpu_seconds"`
	Pids       int64    `json:"pids"`
	// OOMEvents counts how often this acgroup's own memory limit was hit
	// and an allocation was about to fail. Limits of ancestors do not count.
	OOMEvents int64     `json:"oom_events"`
	SampledAt time.Time `json:"sampled_at"`
}

// Lease is one node of an agent tree.
type Lease struct {
	ID          LeaseID      `json:"id"`
	Parent      LeaseID      `json:"parent,omitempty"`
	Lead        LeaseID      `json:"lead"`
	Kind        string       `json:"kind,omitempty"`
	Depth       int          `json:"depth"`
	State       State        `json:"state"`
	Envelope    Envelope     `json:"envelope"`
	UID         int          `json:"uid"`
	OwnerUID    int          `json:"owner_uid"`
	CreatedAt   time.Time    `json:"created_at"`
	Children    []LeaseID    `json:"children"`
	KillOrder   *int         `json:"kill_order,omitempty"`
	OnNearLimit *OnNearLimit `json:"on_near_limit,omitempty"`
	Usage       *Usage       `json:"usage,omitempty"`
	Termination *Termination `json:"termination,omitempty"`
	// Scratch is the lease's scratch directory: its stdout and stderr files
	// and whatever it leaves there for its planner.
	Scratch string `json:"scratch,omitempty"`
	// Activity is the last progress beacon; absent for an agent without an
	// adapter.
	Activity *Activity `json:"activity,omitempty"`
}

// Activity is what an agent last reported about itself.
type Activity struct {
	State string    `json:"state"` // working, awaiting_input or done
	Tool  string    `json:"tool,omitempty"`
	Seq   int64     `json:"seq,omitempty"`
	At    time.Time `json:"at"`
}

// LeaseList is the body of GET /v1/leases.
type LeaseList struct {
	Leases []Lease `json:"leases"`
}

// Termination is the single structured record a lease produces when it ends.
type Termination struct {
	Code        int            `json:"code"`
	Name        string         `json:"name"`
	Lease       LeaseID        `json:"lease"`
	Kind        string         `json:"kind,omitempty"`
	Parent      LeaseID        `json:"parent,omitempty"`
	Dimension   string         `json:"dimension,omitempty"`
	MemBasis    MemBasis       `json:"mem_basis,omitempty"`
	Peak        int64          `json:"peak"`
	Limit       int64          `json:"limit"`
	Enforcement Level          `json:"enforcement,omitempty"`
	Evidence    map[string]any `json:"evidence,omitempty"`
	Message     string         `json:"message"`
	At          time.Time      `json:"at"`
}

// SpawnRequest is the body of POST /v1/leases/{id}/spawn.
type SpawnRequest struct {
	Argv   []string `json:"argv"`
	Env    []string `json:"env,omitempty"`
	Dir    string   `json:"dir,omitempty"`
	DryRun bool     `json:"dry_run,omitempty"`
}

// SpawnResponse reports the main process of the lease.
type SpawnResponse struct {
	Pid int `json:"pid"`
}

// TerminateRequest is the body of POST /v1/leases/{id}/terminate.
// GraceSeconds nil means the default; Code is 204 or 499 (default 499).
type TerminateRequest struct {
	Reason       string   `json:"reason,omitempty"`
	GraceSeconds *float64 `json:"grace_seconds,omitempty"`
	Code         int      `json:"code,omitempty"`
	Recursive    bool     `json:"recursive,omitempty"`
	DryRun       bool     `json:"dry_run,omitempty"`
}

// KillRequest is the optional body of POST /v1/leases/{id}/kill.
type KillRequest struct {
	DryRun bool `json:"dry_run,omitempty"`
}

// WaitChildrenRequest is the body of POST /v1/leases/{id}/wait-children.
// Self also waits for the lease itself; TimeoutSeconds zero waits forever.
type WaitChildrenRequest struct {
	Self           bool    `json:"self,omitempty"`
	TimeoutSeconds float64 `json:"timeout_seconds,omitempty"`
}

// WaitChildrenResponse lists the termination record of each lease waited for.
type WaitChildrenResponse struct {
	Done    bool          `json:"done"`
	Results []Termination `json:"results"`
}

// Suggestion is a deterministic initial-quota suggestion for one kind.
type Suggestion struct {
	Memory   int64    `json:"memory"`
	Samples  int      `json:"samples"`
	Default  bool     `json:"default"`
	MemBasis MemBasis `json:"mem_basis"`
}

// Available is the remaining budget of the caller's subtree.
type Available struct {
	Lease                 LeaseID               `json:"lease"`
	MemBasis              MemBasis              `json:"mem_basis"`
	MemoryLimit           int64                 `json:"memory_limit"`
	MemorySelf            int64                 `json:"memory_self"`
	MemoryAllocated       int64                 `json:"memory_allocated"`
	MemoryFree            int64                 `json:"memory_free"`
	LiveChildren          int                   `json:"live_children"`
	HostEffectiveCapacity int64                 `json:"host_effective_capacity"`
	Suggest               map[string]Suggestion `json:"suggest"`
}

// Error is the body of every non-2xx response. Field, Expected and Example
// are filled for validation failures so a caller can correct itself.
type Error struct {
	Code     string `json:"error"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message"`
	Field    string `json:"field,omitempty"`
	Expected string `json:"expected,omitempty"`
	Example  string `json:"example,omitempty"`
}

func (e *Error) Error() string {
	s := e.Code
	if e.Reason != "" {
		s += " (" + e.Reason + ")"
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Error codes carried in Error.Code.
const (
	ErrInvalidArgument = "invalid_argument"
	ErrRejected        = "rejected"
	ErrOutOfSubtree    = "out_of_subtree"
	ErrSelfResize      = "self_resize_forbidden"
	ErrNotFound        = "not_found"
	ErrConflict        = "conflict"
	ErrNotImplemented  = "not_implemented"
	ErrInternal        = "internal"
)

// Reject reasons carried in Error.Reason when Code is ErrRejected.
const (
	ReasonMemBudget              = "mem_budget"
	ReasonParentBudget           = "parent_budget"
	ReasonEnforcementUnavailable = "enforcement_unavailable"
	ReasonParentNotRunning       = "parent_not_running"
	ReasonCapacityShrunk         = "capacity_shrunk"
	ReasonBelowUsage             = "below_usage"
	ReasonBelowAllocated         = "below_allocated"
)

// Schemas maps each schema name in openapi.yaml to its Go type. The
// conformance test compares JSON field names in both directions.
var Schemas = map[string]any{
	"Capabilities":         Capabilities{},
	"Host":                 Host{},
	"Envelope":             Envelope{},
	"EnvelopePatch":        EnvelopePatch{},
	"Require":              Require{},
	"OnNearLimit":          OnNearLimit{},
	"AcquireRequest":       AcquireRequest{},
	"Usage":                Usage{},
	"Lease":                Lease{},
	"Activity":             Activity{},
	"LeaseList":            LeaseList{},
	"Termination":          Termination{},
	"SpawnRequest":         SpawnRequest{},
	"SpawnResponse":        SpawnResponse{},
	"TerminateRequest":     TerminateRequest{},
	"KillRequest":          KillRequest{},
	"WaitChildrenRequest":  WaitChildrenRequest{},
	"WaitChildrenResponse": WaitChildrenResponse{},
	"Suggestion":           Suggestion{},
	"Available":            Available{},
	"Error":                Error{},
}

// The types below belong to endpoints that land in later milestones. They
// are defined now so the contract is complete from the start.

// ResizeAsk is the body of POST /v1/leases/{id}/resize-request.
type ResizeAsk struct {
	Memory int64 `json:"memory"`
	Usage  int64 `json:"usage,omitempty"`
}

// Control message types. The set is closed.
const (
	MsgTerminateRequested = "TERMINATE_REQUESTED"
	MsgNearLimit          = "NEAR_LIMIT"
	MsgResized            = "RESIZED"
	MsgCapacityShrunk     = "CAPACITY_SHRUNK"
)

// NotifyRequest is the body of POST /v1/leases/{id}/notify.
type NotifyRequest struct {
	Type   string `json:"type"`
	Reason string `json:"reason,omitempty"`
	DryRun bool   `json:"dry_run,omitempty"`
}

// ProgressRequest is a progress beacon reported by a harness adapter.
type ProgressRequest struct {
	State  string `json:"state"`
	Tool   string `json:"tool,omitempty"`
	Seq    int64  `json:"seq,omitempty"`
	Turns  int64  `json:"turns,omitempty"`
	Result string `json:"result,omitempty"`
	Tokens int64  `json:"tokens,omitempty"`
}

// InboxMessage is one control message delivered to a lease.
type InboxMessage struct {
	Seq      int64      `json:"seq"`
	Type     string     `json:"type"`
	Source   string     `json:"source"`
	Reason   string     `json:"reason,omitempty"`
	Deadline *time.Time `json:"deadline,omitempty"`
	At       time.Time  `json:"at"`
}

// Event is one entry of the event stream.
type Event struct {
	Seq   int64          `json:"seq"`
	Type  string         `json:"type"`
	Lease LeaseID        `json:"lease,omitempty"`
	At    time.Time      `json:"at"`
	Data  map[string]any `json:"data,omitempty"`
}

// Trace is the timeline of one lease.
type Trace struct {
	Lease       LeaseID      `json:"lease"`
	Samples     []Usage      `json:"samples"`
	Events      []Event      `json:"events"`
	Termination *Termination `json:"termination,omitempty"`
	// Spikes are the tool calls during which memory grew by at least
	// SpikeMin, largest growth first: what the timeline says about where
	// a jump in memory came from.
	Spikes []Spike `json:"spikes,omitempty"`
	// Children are the timelines of the lease's children, each with its
	// own children, when the whole subtree was asked for.
	Children []Trace `json:"children,omitempty"`
}

// Spike is the memory growth during one tool call: from its progress
// beacon to the next one.
type Spike struct {
	Tool   string    `json:"tool"`
	Seq    int64     `json:"seq"`
	From   time.Time `json:"from"`
	Until  time.Time `json:"until,omitzero"` // zero when no beacon followed
	Before int64     `json:"before"`         // memory when the call began, bytes
	Peak   int64     `json:"peak"`           // highest memory during the call, bytes
	Growth int64     `json:"growth"`         // Peak - Before
}

// SpikeMin is the smallest growth reported as a spike.
const SpikeMin = 64 << 20

func init() {
	Schemas["ResizeAsk"] = ResizeAsk{}
	Schemas["NotifyRequest"] = NotifyRequest{}
	Schemas["ProgressRequest"] = ProgressRequest{}
	Schemas["InboxMessage"] = InboxMessage{}
	Schemas["Event"] = Event{}
	Schemas["Trace"] = Trace{}
	Schemas["Spike"] = Spike{}
}

// Proc is one process of a lease, as listed in the evidence of a
// termination under "top_procs": the processes using the most memory
// shortly before the end. Memory is the process's resident set on Linux
// and its physical footprint on macOS.
type Proc struct {
	Pid    int    `json:"pid"`
	Name   string `json:"name"`
	Memory int64  `json:"memory"`
}
