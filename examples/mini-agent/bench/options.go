package bench

import (
	"time"

	"github.com/zydo/subactl/examples/mini-agent/scenarios"
)

// Options describes one run.
type Options struct {
	Group        string // A, B or C
	RunIndex     int
	ScenarioPath string
	Scenario     *scenarios.Scenario
	Mode         string // mini-agent mode: without or with
	MiniAgent    string // path of the mini-agent binary
	Self         string // path of the bench binary, for the interference process
	OutDir       string // where this run's files go
	WorkDir      string // working directory of the lead
	Base         string // parent cgroup of the chambers
	ChamberBytes int64
	UID, GID     int    // the lead never runs as root
	Home         string // HOME of that user
	// With subactl (groups B and C): where subactld and subactl are, the
	// reserve line inside the chamber, the daemon's state directory, the
	// group that may use its socket, and whether workers size themselves
	// from history.
	BinDir      string
	Reserve     int64
	StateDir    string
	SocketGroup string
	Adaptive    bool
	Settle      time.Duration
	Log         func(format string, a ...any)
}
