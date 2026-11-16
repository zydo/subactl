// Command mini-agent is the smallest multi-agent harness that can exercise
// subactl. A person runs it to get a lead agent; an agent runs it from its
// bash tool to get a child agent.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/client"
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
	// envWrapped marks a process that subactl started for mini-agent. It is
	// removed from the environment of the bash tool, so that a mini-agent
	// started from there wraps itself again, as a child.
	envWrapped  = "MINI_AGENT_WRAPPED"
	envAdaptive = "MINI_AGENT_ADAPTIVE" // "1" or "0": overrides the scenario's adaptive
	envLease    = "SUBACTL_LEASE_ID"    // set by subactl inside a lease
	envSocket   = "SUBACTL_SOCK"
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

	if *mode != "with" && *mode != "without" {
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
	// The one switch between the two modes. With subactl, mini-agent does
	// not start itself: it asks subactl to, as a lead when it is outside
	// every lease and as a child of the current lease when it is inside
	// one. A model that writes "mini-agent --task X &" in bash therefore
	// still ends up inside the agent tree. Everything below is shared.
	if v := os.Getenv(envAdaptive); v != "" {
		sc.Adaptive = v == "1" // the bench runs one scenario file as group B or C
	}
	if *mode == "with" && os.Getenv(envWrapped) == "" {
		code, err := wrap(sc, self, *task, depth, agent.Registry{Dir: *runDir})
		if err != nil {
			return fail(exitFailed, "%v", err)
		}
		return code
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

	// The inherited environment first, then this agent's own task values,
	// which must win over what its parent exported.
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, envWrapped+"=") {
			env = append(env, kv)
		}
	}
	env = append(env, sc.TaskEnv(*task, depth)...)
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

	var adapter *agent.Adapter
	var mirror io.Writer
	if *mode == "with" {
		adapter = agent.NewAdapter(os.Getenv(envSocket))
		// A child's stdout is its lease's output file; show running
		// commands there, as a terminal would. The lead's is the user's
		// terminal, which already shows the events.
		if !lead {
			mirror = os.Stdout
		}
	}
	a := &agent.Agent{
		Brain:   brain,
		Tools:   tools.All(tools.Config{Dir: cwd, SpillDir: work, Env: env, Mirror: mirror}),
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
	if adapter != nil {
		adapter.Attach(ctx, a, api.LeaseID(os.Getenv(envLease)))
	}
	err = a.Run(ctx, "Carry out the task: "+label)
	// Under subactl the outcome is also handed up through it: a result file
	// in the lease's scratch directory and a "done" beacon.
	report := func(res agent.Result) {
		_ = reg.WriteResult(res)
		if adapter != nil {
			data, _ := json.Marshal(res)
			_ = adapter.Done(context.Background(), string(data))
		}
	}
	switch {
	case errors.Is(err, agent.ErrStop):
		printer.Line("asked to terminate: stopping before the next tool call")
		report(agent.Result{Task: workName, Status: "failed", Detail: "terminated"})
		return exitFailed
	case errors.Is(err, llm.ErrQuota):
		// An abort is not a verdict on the task: no result is written, so
		// the planner sees the task as lost and dispatches it again.
		printer.Line("giving up: " + err.Error())
		return exitQuota
	case errors.Is(err, context.Canceled):
		return 130
	case err != nil:
		report(agent.Result{Task: workName, Status: "failed", Detail: err.Error()})
		return fail(exitFailed, "%v", err)
	}
	if lead {
		printer.Line(fmt.Sprintf("finished; run directory: %s", *runDir))
		if adapter != nil {
			_ = adapter.Done(context.Background(), `{"status":"ok"}`)
		}
		return 0
	}
	// A task succeeded if its last step left the ok marker.
	res := agent.Result{Task: workName, Status: "ok"}
	if _, err := os.Stat(filepath.Join(work, "ok")); err != nil {
		res.Status = "failed"
	}
	report(res)
	// Under subactl the verdict travels in the result, and the lease itself
	// completed: a non-zero exit would read as an abnormal end.
	if res.Status != "ok" && adapter == nil {
		return exitFailed
	}
	return 0
}

// wrap asks subactl to start the same mini-agent command.
//
// Outside every lease it becomes "subactl run": the process is replaced,
// and subactl is the lead's holder. Inside a lease it runs
// "subactl spawn --wait" and stays as a small waiting process, because two
// things have to happen around the child: an admission that is refused
// for lack of room is tried again, and the exit status has to carry the
// task's verdict so that "tests || mini-agent --task fix" works in bash.
func wrap(sc *scenarios.Scenario, self, task string, depth int, reg agent.Registry) (int, error) {
	subactl, err := exec.LookPath("subactl")
	if err != nil {
		// Fall back to a subactl installed next to mini-agent.
		subactl = filepath.Join(filepath.Dir(self), "subactl")
		if _, serr := os.Stat(subactl); serr != nil {
			return 0, fmt.Errorf("subactl is not on PATH; mini-agent --mode with needs it (%v)", err)
		}
	}
	env := append(os.Environ(), envWrapped+"=1")
	if os.Getenv(envLease) == "" {
		args := []string{"subactl", "run", "--kind", "lead"}
		if m := sc.Envelopes["lead"]; m > 0 {
			args = append(args, "--memory", strconv.FormatInt(m, 10))
		}
		args = append(append(args, "--", self), os.Args[1:]...)
		return 0, syscall.Exec(subactl, args, env)
	}
	kind, _, _ := strings.Cut(task, ":")
	if kind == "" {
		return 0, errors.New("inside a lease a mini-agent needs --task")
	}
	ns, _ := strconv.ParseInt(os.Getenv(envT0), 10, 64)
	deadline := time.Unix(0, ns).Add(sc.Duration)
	for attempt := 0; ; attempt++ {
		// The envelope. A static scenario names it per kind. An adaptive
		// one starts from what subactl suggests from the peaks of earlier
		// agents of the same kind, and declares how far the limit may grow.
		memory := sc.Envelopes[kind]
		if sc.Adaptive {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			a, err := client.New(os.Getenv(envSocket)).Available(ctx, api.Self, kind)
			cancel()
			if s, ok := a.Suggest[kind]; err == nil && ok && !s.Default {
				memory = s.Memory
			}
		}
		if memory == 0 {
			memory = 1 << 30
		}
		args := []string{"spawn", "--wait", "--kind", kind, "--memory", strconv.FormatInt(memory, 10)}
		if sc.Adaptive && sc.GrowFactor > 1 {
			args = append(args, "--grow-to", strconv.FormatInt(int64(float64(memory)*sc.GrowFactor), 10))
		}
		if sc.OutputLimit > 0 {
			args = append(args, "--output", strconv.FormatInt(sc.OutputLimit, 10))
		}
		args = append(append(args, "--", self), os.Args[1:]...)
		cmd := exec.Command(subactl, args...)
		cmd.Env = env
		var stderr strings.Builder
		cmd.Stdout, cmd.Stderr = os.Stdout, &stderr
		runErr := cmd.Run()
		// subactl exits 1 when the request itself was refused. A refusal
		// for lack of room is not a failure of the task: the planner asked
		// for more than fits right now, so wait for room and ask again.
		var refusal api.Error
		_ = json.Unmarshal([]byte(stderr.String()), &refusal)
		if runErr != nil && refusal.Code == api.ErrRejected && time.Now().Before(deadline) {
			switch refusal.Reason {
			case api.ReasonParentBudget, api.ReasonMemBudget, api.ReasonCapacityShrunk:
				time.Sleep(time.Duration(2000+(os.Getpid()+attempt*37)%2000) * time.Millisecond)
				continue
			}
		}
		fmt.Fprint(os.Stderr, stderr.String())
		break
	}
	// The verdict is the result the child wrote, not how its lease ended.
	want := fmt.Sprintf("%s.d%d", task, depth)
	for _, r := range reg.Results() {
		if r.Task == want && r.Status == "ok" {
			return 0, nil
		}
	}
	return exitFailed, nil
}
