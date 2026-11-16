// Command mini-agent is the smallest multi-agent harness that can exercise
// subactl. A person runs it to get a lead agent; an agent runs it from its
// bash tool to get a child agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/zydo/subactl/examples/mini-agent/agent"
	"github.com/zydo/subactl/examples/mini-agent/llm"
	"github.com/zydo/subactl/examples/mini-agent/scenarios"
	"github.com/zydo/subactl/examples/mini-agent/tools"
	"github.com/zydo/subactl/examples/mini-agent/tui"
)

// Environment shared by the agents of one run. Children inherit it.
const (
	envRun      = "MINI_AGENT_RUN"      // run directory
	envT0       = "MINI_AGENT_T0"       // scenario clock origin, Unix nanoseconds
	envDepth    = "MINI_AGENT_DEPTH"    // 0 for the lead
	envParent   = "MINI_AGENT_PARENT"   // pid of the parent agent
	envScenario = "MINI_AGENT_SCENARIO" // scenario file
	envMode     = "MINI_AGENT_MODE"     // "with" or "without"
	// envRunID marks every process of a run, so that what is left behind
	// can be found afterwards.
	envRunID = "SUBACTL_EXPERIMENT"
)

// Exit statuses.
const (
	exitFailed = 1  // the task did not succeed
	exitUsage  = 2  // bad invocation
	exitQuota  = 75 // gave up on the usage limit (EX_TEMPFAIL)
)

func main() { os.Exit(run()) }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func run() int {
	task := flag.String("task", "", "task for a child agent, e.g. upgrade:dialog (empty for the lead)")
	scenarioPath := flag.String("scenario", os.Getenv(envScenario), "scenario file for the scripted brain")
	mode := flag.String("mode", envOr(envMode, "with"), "\"with\" runs under subactl; \"without\" starts children as plain processes")
	runDir := flag.String("run-dir", os.Getenv(envRun), "run directory (default: a new temporary directory)")
	brainName := flag.String("brain", "scripted", "scripted or real")
	flag.Parse()
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "mini-agent: "+format+"\n", a...)
		return code
	}

	// The one switch between the two modes. Everything below is shared.
	switch *mode {
	case "without":
	case "with":
		return fail(exitUsage, "self-wrapping under subactl lands in milestone M4; use --mode without")
	default:
		return fail(exitUsage, "unknown mode %q: want with or without", *mode)
	}
	if *brainName != "scripted" {
		return fail(exitUsage, "the %s brain lands in milestone M5; use --brain scripted", *brainName)
	}
	if *scenarioPath == "" {
		return fail(exitUsage, "the scripted brain needs --scenario FILE")
	}
	sc, err := scenarios.Load(*scenarioPath)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	depth, _ := strconv.Atoi(os.Getenv(envDepth))
	parent, _ := strconv.Atoi(os.Getenv(envParent))
	lead := depth == 0
	if lead != (*task == "") {
		return fail(exitUsage, "a lead takes no --task and a child agent needs one (depth %d, task %q)", depth, *task)
	}
	self, err := os.Executable()
	if err != nil {
		return fail(exitFailed, "%v", err)
	}
	abs, err := filepath.Abs(*scenarioPath)
	if err != nil {
		return fail(exitFailed, "%v", err)
	}

	// The lead creates the run; children find it in their environment.
	t0 := time.Now()
	if lead {
		if *runDir == "" {
			if *runDir, err = os.MkdirTemp("", "mini-agent-run-"); err != nil {
				return fail(exitFailed, "%v", err)
			}
		}
		if err := sc.Extract(filepath.Join(*runDir, "task")); err != nil {
			return fail(exitFailed, "%v", err)
		}
	} else {
		ns, err := strconv.ParseInt(os.Getenv(envT0), 10, 64)
		if err != nil || *runDir == "" {
			return fail(exitUsage, "a child agent must be started by another mini-agent (%s and %s are missing)", envRun, envT0)
		}
		t0 = time.Unix(0, ns)
	}
	clock := func() time.Duration { return time.Since(t0) }
	reg := agent.Registry{Dir: *runDir}
	pid := os.Getpid()
	// Fixers at different depths share a task name, so the depth is part of
	// the scratch directory's name.
	label := *task
	if lead {
		label = "lead"
	}
	workName := fmt.Sprintf("%s.d%d", label, depth)
	if err := reg.Register(agent.Entry{Pid: pid, Parent: parent, Depth: depth, Task: workName}); err != nil {
		return fail(exitFailed, "%v", err)
	}
	defer reg.Unregister(pid)
	work := reg.WorkDir(workName)

	steps, err := sc.Script(*task, depth)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	brain := &llm.Scripted{
		Steps: steps, Clock: clock,
		QuotaAt: sc.QuotaAt, QuotaFor: sc.QuotaFor, LeakBytes: int64(sc.LeakMB) << 20,
	}

	env := append(os.Environ(), sc.TaskEnv(*task, depth)...)
	env = append(env,
		envRun+"="+*runDir,
		fmt.Sprint(envT0, "=", t0.UnixNano()),
		fmt.Sprint(envDepth, "=", depth+1),
		fmt.Sprint(envParent, "=", pid),
		envScenario+"="+abs,
		envMode+"="+*mode,
		envRunID+"="+envOr(envRunID, filepath.Base(*runDir)),
		"RUN_ID="+envOr(envRunID, filepath.Base(*runDir)),
		"RUN_DIR="+*runDir,
		"TASK_DIR="+filepath.Join(*runDir, "task"),
		"WORK="+work,
		"MINI_AGENT="+self,
	)
	cwd, _ := os.Getwd()

	transcript, err := os.Create(filepath.Join(work, "transcript.log"))
	if err != nil {
		return fail(exitFailed, "%v", err)
	}
	defer transcript.Close()
	printer := tui.NewPrinter(label, clock, os.Stdout, transcript)

	// killSubagents is the harness's reaction to the usage limit: SIGKILL
	// each running subagent, and only the subagent. In this mode whatever
	// the subagent started is left behind.
	killSubagents := func() {
		for _, k := range reg.KillChildren(pid) {
			printer.Line(fmt.Sprintf("usage limit: killed subagent %s (pid %d)", k.Task, k.Pid))
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if sc.QuotaFor > 0 {
		// The limit is account-wide and arrives whatever the planner is
		// doing, including while it waits for its workers in a tool call.
		go func() {
			select {
			case <-ctx.Done():
			case <-time.After(sc.QuotaAt - clock()):
				killSubagents()
			}
		}()
	}

	a := &agent.Agent{
		Brain:   brain,
		Tools:   tools.All(tools.Config{Dir: cwd, SpillDir: work, Env: env}),
		System:  "You are mini-agent. Tools: read, write, edit, bash.",
		OnEvent: printer.Event,
		OnQuota: func(context.Context) (time.Duration, bool) {
			killSubagents()
			if !lead {
				return 0, false // a subagent cannot outwait the limit
			}
			return max(sc.QuotaAt+sc.QuotaFor-clock(), 0) + time.Second, true
		},
	}
	err = a.Run(ctx, "Carry out the task: "+label)
	switch {
	case errors.Is(err, llm.ErrQuota):
		printer.Line("giving up: " + err.Error())
		_ = reg.WriteResult(agent.Result{Task: workName, Status: "failed", Detail: "usage_limit"})
		return exitQuota
	case errors.Is(err, context.Canceled):
		return 130
	case err != nil:
		_ = reg.WriteResult(agent.Result{Task: workName, Status: "failed", Detail: err.Error()})
		return fail(exitFailed, "%v", err)
	}
	if lead {
		printer.Line(fmt.Sprintf("finished; run directory: %s", *runDir))
		return 0
	}
	// A task succeeded if its last step left the ok marker.
	res := agent.Result{Task: workName, Status: "ok"}
	if _, err := os.Stat(filepath.Join(work, "ok")); err != nil {
		res.Status = "failed"
	}
	if err := reg.WriteResult(res); err != nil {
		return fail(exitFailed, "%v", err)
	}
	if res.Status != "ok" {
		return exitFailed
	}
	return 0
}
