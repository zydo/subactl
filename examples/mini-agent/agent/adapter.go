package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/client"
)

// Adapter connects an Agent to subactl. It is the reference harness
// adapter of the design: on the loop's hooks it reports progress beacons,
// injects the lease's control messages into the conversation, and passes
// what happened to the agent's children up to it, once per turn.
//
// None of this goes through the model: beacons cost no tokens.
type Adapter struct {
	c *client.Client

	terminate atomic.Bool
	mu        sync.Mutex
	// pending counts the events about descendants since the last turn, by
	// lease and type, so that a hundred of a kind become one line.
	pending map[[2]string]int
	turns   int
}

// NewAdapter returns an adapter for the lease this process runs in.
func NewAdapter(socket string) *Adapter {
	return &Adapter{c: client.New(socket), pending: map[[2]string]int{}}
}

// Attach installs the adapter's hooks on the agent and starts listening.
// It returns when ctx ends.
func (ad *Adapter) Attach(ctx context.Context, a *Agent, self api.LeaseID) {
	a.BeforeTool = func(ctx context.Context, tool string, seq int) error {
		// After TERMINATE_REQUESTED no new tool is started.
		if ad.terminate.Load() {
			return ErrStop
		}
		// Only the tool's name is reported, never its arguments.
		_ = ad.c.Progress(ctx, api.Self, api.ProgressRequest{State: "working", Tool: tool, Seq: int64(seq), Turns: int64(ad.turns)})
		return nil
	}
	a.AfterTurn = func(ctx context.Context, turn int) {
		ad.turns = turn
		_ = ad.c.Progress(ctx, api.Self, api.ProgressRequest{State: "working", Turns: int64(turn), Tokens: a.Tokens()})
		if s := ad.summary(); s != "" {
			a.Steer(s)
		}
	}
	// The lease's own inbox: messages from subactld and from ancestors.
	go func() {
		_ = ad.c.Inbox(ctx, api.Self, func(m api.InboxMessage) error {
			if m.Type == api.MsgTerminateRequested {
				ad.terminate.Store(true)
			}
			a.Steer(render(m))
			return nil
		})
	}()
	// Events about the subtree: what happened to the children.
	go func() {
		_ = ad.c.Events(ctx, func(e api.Event) error {
			if e.Lease == "" || e.Lease == self || e.Type == "STATE" {
				return nil
			}
			ad.mu.Lock()
			ad.pending[[2]string{string(e.Lease), e.Type}]++
			ad.mu.Unlock()
			return nil
		})
	}()
}

// AwaitingInput tells subactl that the agent is waiting for a person, which
// is not idleness.
func (ad *Adapter) AwaitingInput(ctx context.Context) {
	_ = ad.c.Progress(ctx, api.Self, api.ProgressRequest{State: "awaiting_input", Turns: int64(ad.turns)})
}

// Terminating reports whether the lease was asked to end.
func (ad *Adapter) Terminating() bool { return ad.terminate.Load() }

// render shows a control message in a fixed template. The source is the
// one subactld attached; the message's own text cannot change it.
func render(m api.InboxMessage) string {
	s := fmt.Sprintf("[subactl control message from %s] %s", m.Source, m.Type)
	if m.Deadline != nil {
		s += " by " + m.Deadline.Format("15:04:05")
	}
	if m.Reason != "" {
		s += fmt.Sprintf(" (reason given: %q)", m.Reason)
	}
	if m.Type == api.MsgTerminateRequested {
		s += ". Save your progress, end your own child agents and exit."
	}
	return s
}

// summary drains the pending events into one message: one line per lease
// and type, however many events there were.
func (ad *Adapter) summary() string {
	ad.mu.Lock()
	defer ad.mu.Unlock()
	if len(ad.pending) == 0 {
		return ""
	}
	lines := make([]string, 0, len(ad.pending))
	for k, n := range ad.pending {
		line := fmt.Sprintf("%s %s", k[0], k[1])
		if n > 1 {
			line += fmt.Sprintf(" (x%d)", n)
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	ad.pending = map[[2]string]int{}
	return "[subactl events about your child agents since the last turn]\n" + strings.Join(lines, "\n")
}

// Done hands the result file to the planner and reports the task done.
// The file must be in the lease's scratch directory.
func (ad *Adapter) Done(ctx context.Context, content string) error {
	scratch := os.Getenv("SUBACTL_SCRATCH")
	if scratch == "" {
		return ad.c.Progress(ctx, api.Self, api.ProgressRequest{State: "done"})
	}
	path := filepath.Join(scratch, "result.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	return ad.c.Progress(ctx, api.Self, api.ProgressRequest{State: "done", Result: path})
}
