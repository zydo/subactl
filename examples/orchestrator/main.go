// Command orchestrator is the tutorial's lead written as code
// (docs/tutorial.md). It does what fanout.sh does, through the Go client:
// one worker per task, as many at once as its own memory allows, each
// sized by subactl's suggestion; and it reacts to how each worker ended.
//
//	go build -o bin/orchestrator ./examples/orchestrator
//	subactl run --memory 4G -- bin/orchestrator -worker examples/orchestrator/worker.sh a b c leak server d
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/client"
)

func main() {
	worker := flag.String("worker", "examples/orchestrator/worker.sh", "the worker command; it gets the task as its argument")
	flag.Parse()
	if os.Getenv("SUBACTL_LEASE_ID") == "" {
		fmt.Fprintln(os.Stderr, "orchestrator: run me as a lead: subactl run --memory 4G -- orchestrator TASK...")
		os.Exit(2)
	}
	// The daemon knows who we are from our process: no token, no id needed.
	c := client.New(client.SocketFromEnv())
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Stop when the operator or an ancestor asks us to. Our workers need no
	// cleanup code: when this process exits, subactl ends them all (502).
	go func() {
		_ = c.Inbox(ctx, api.Self, func(m api.InboxMessage) error {
			if m.Type == api.MsgTerminateRequested {
				cancel()
			}
			return nil
		})
	}()

	tasks := flag.Args()
	ended := make(chan api.Termination)
	for _, task := range tasks {
		go func() { ended <- runTask(ctx, c, *worker, task) }()
	}
	for range tasks {
		t := <-ended
		fmt.Printf("%s: %d %s, peak %d MiB of %d MiB\n", t.Kind, t.Code, t.Name, t.Peak>>20, t.Limit>>20)
	}
}

// runTask runs one task to its end. The policy is the planner's, not
// subactl's: wait when there is no room, give a worker that went over its
// limit one more try with twice the memory, retry one ended by the safety
// net later, and leave everything else as it ended.
func runTask(ctx context.Context, c *client.Client, worker, task string) api.Termination {
	memory := int64(0)
	for attempt := 1; ; attempt++ {
		if memory == 0 {
			av, err := c.Available(ctx, api.Self, "worker")
			if err != nil {
				return failed(task, err)
			}
			memory = av.Suggest["worker"].Memory
		}
		t, err := runOnce(ctx, c, worker, task, memory)
		var refusal *api.Error
		switch {
		case errors.As(err, &refusal) && retryable(refusal.Reason):
			time.Sleep(time.Second) // no room yet; ask again
			attempt--
			continue
		case err != nil:
			return failed(task, err)
		case t.Code == api.CodeOOMLeaseLimit && attempt < 2:
			memory *= 2
			continue
		case t.Code == api.CodeSafetyNet && attempt < 3:
			time.Sleep(10 * time.Second)
			continue
		}
		t.Kind = task
		return t
	}
}

// runOnce takes a child lease, starts the worker in it and waits for it.
func runOnce(ctx context.Context, c *client.Client, worker, task string, memory int64) (api.Termination, error) {
	ls, err := c.Acquire(ctx, api.AcquireRequest{
		// No parent: a child of the lease this process runs in.
		Kind:     "worker",
		Envelope: api.Envelope{Memory: memory, Wall: 300},
	})
	if err != nil {
		return api.Termination{}, err
	}
	cwd, _ := os.Getwd()
	if _, err := c.Spawn(ctx, ls.ID, api.SpawnRequest{Argv: []string{worker, task}, Env: os.Environ(), Dir: cwd}); err != nil {
		_, _ = c.Kill(context.Background(), ls.ID)
		return api.Termination{}, err
	}
	res, err := c.WaitChildren(ctx, ls.ID, api.WaitChildrenRequest{Self: true})
	if err != nil {
		return api.Termination{}, err
	}
	for _, t := range res.Results {
		if t.Lease == ls.ID {
			return t, nil
		}
	}
	return api.Termination{}, fmt.Errorf("no termination record for %s", ls.ID)
}

func retryable(reason string) bool {
	switch reason {
	case api.ReasonParentBudget, api.ReasonMemBudget, api.ReasonCapacityShrunk:
		return true
	}
	return false
}

func failed(task string, err error) api.Termination {
	return api.Termination{Kind: task, Code: -1, Name: "NOT_RUN", Message: err.Error()}
}
