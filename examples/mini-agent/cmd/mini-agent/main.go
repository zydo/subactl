// Command mini-agent is the smallest multi-agent harness that can exercise
// subactl. A person runs it to get a lead agent; an agent runs it from its
// bash tool to get a child agent.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

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
	envBrain    = "MINI_AGENT_BRAIN"    // scripted or real; children use their parent's
	envProvider = "MINI_AGENT_PROVIDER" // endpoint of the real brain
	envModel    = "MINI_AGENT_MODEL"    // model of the real brain
	envBaseURL  = "MINI_AGENT_BASE_URL" // API root of the real brain
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
	brainName := flag.String("brain", envOr(envBrain, "scripted"), "scripted or real")
	providerName := flag.String("provider", envOr(envProvider, "anthropic"), "endpoint of the real brain: anthropic, deepseek or zai")
	model := flag.String("model", os.Getenv(envModel), "model of the real brain (default: the provider's)")
	baseURL := flag.String("base-url", os.Getenv(envBaseURL), "API root of the real brain, e.g. a proxy (default: the provider's)")
	prompt := flag.String("prompt", "", "what the lead should do, with the real brain (default: read it from the terminal)")
	memoryFlag := flag.String("memory", "", "memory limit of this child agent under subactl, e.g. 2G (default: the scenario's, or subactl's suggestion)")
	growFlag := flag.String("grow-to", "", "how far this child's limit may grow on a near-limit warning, e.g. 3G")
	flag.Parse()
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "mini-agent: "+format+"\n", a...)
		return code
	}

	if *mode != "with" && *mode != "without" {
		return fail(exitUsage, "unknown mode %q: want with or without", *mode)
	}
	real := *brainName == "real"
	if !real && *brainName != "scripted" {
		return fail(exitUsage, "unknown brain %q: want scripted or real", *brainName)
	}
	var memory, growTo int64
	for _, f := range []struct {
		val string
		dst *int64
	}{{*memoryFlag, &memory}, {*growFlag, &growTo}} {
		if f.val != "" {
			n, err := api.ParseBytes(f.val)
			if err != nil || n <= 0 {
				return fail(exitUsage, "bad size %q: want e.g. 2G or 512M", f.val)
			}
			*f.dst = n
		}
	}
	// A scenario may give the agents below the lead a brain of their own.
	if os.Getenv(envDepth) != "" && os.Getenv(envDepth) != "0" && *scenarioPath != "" {
		if wsc, err := scenarios.Load(*scenarioPath); err == nil && wsc.WorkerBrain != "" {
			real = wsc.WorkerBrain == "real"
		}
	}
	var provider llm.Provider
	if real {
		for _, p := range llm.Providers {
			if p.Name == *providerName {
				provider = p
			}
		}
		if provider.Name == "" {
			return fail(exitUsage, "unknown provider %q: want anthropic, deepseek or zai", *providerName)
		}
		if os.Getenv(provider.KeyEnv) == "" {
			return fail(exitUsage, "the %s provider reads its key from %s, which is not set", provider.Name, provider.KeyEnv)
		}
	} else if *scenarioPath == "" {
		return fail(exitUsage, "the scripted brain needs --scenario FILE")
	}
	sc := scenarios.Blank()
	var err error
	if *scenarioPath != "" {
		if sc, err = scenarios.Load(*scenarioPath); err != nil {
			return fail(exitUsage, "%v", err)
		}
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
		code, err := wrap(sc, self, *task, depth, agent.Registry{Dir: *runDir}, memory, growTo)
		if err != nil {
			return fail(exitFailed, "%v", err)
		}
		return code
	}
	abs := ""
	if *scenarioPath != "" {
		if abs, err = filepath.Abs(*scenarioPath); err != nil {
			return fail(exitFailed, "%v", err)
		}
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
	label := labelOf(*task)
	if lead {
		label = "lead"
	}
	workName := fmt.Sprintf("%s.d%d", label, depth)
	if err := reg.Register(agent.Entry{Pid: pid, Parent: parent, Depth: depth, Task: workName}); err != nil {
		return fail(exitFailed, "%v", err)
	}
	defer reg.Unregister(pid)
	work := reg.WorkDir(workName)

	var brain llm.Brain
	if real {
		if *baseURL != "" {
			provider.BaseURL = *baseURL
		}
		brain = llm.NewMessages(provider, *model, os.Getenv(provider.KeyEnv))
	} else {
		steps, err := sc.Script(*task, depth)
		if err != nil {
			return fail(exitUsage, "%v", err)
		}
		brain = &llm.Scripted{
			Steps: steps, Clock: clock,
			QuotaAt: sc.QuotaAt, QuotaFor: sc.QuotaFor, LeakBytes: int64(sc.LeakMB) << 20,
		}
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
		envBrain+"="+*brainName,
		envProvider+"="+*providerName,
		envModel+"="+*model,
		envBaseURL+"="+*baseURL,
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
	// SIGTERM ends the agent. Ctrl-C (SIGINT) first interrupts the turn in
	// progress and returns to the prompt; a second one, with no turn in
	// progress or before the interrupted turn has wound down, ends the agent.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	turns := &turnControl{root: ctx, quit: stop}
	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt)
	defer signal.Stop(sigint)
	go func() {
		for range sigint {
			if !turns.interrupt() {
				stop()
				return
			}
		}
	}()
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
		System:  systemPrompt,
		OnEvent: printer.Event,
		OnQuota: func(context.Context) (time.Duration, bool) {
			killSubagents()
			// A subagent cannot outwait the limit. With a real provider the
			// lead does not try either: the limit may last hours, and
			// asking again every second only adds to the account's
			// traffic. The user is told and decides.
			if !lead || real {
				return 0, false
			}
			return max(sc.QuotaAt+sc.QuotaFor-clock(), 0) + time.Second, true
		},
	}
	if adapter != nil {
		adapter.Attach(ctx, a, api.LeaseID(os.Getenv(envLease)))
	}
	switch {
	case !real:
		err = turns.run(func(tc context.Context) error { return a.Run(tc, "Carry out the task: "+label) })
	case !lead:
		text := *task
		if p := sc.Prompt(*task, sc.Duration-clock()); p != "" {
			text = p
		}
		err = turns.run(func(tc context.Context) error { return a.Run(tc, text) })
	case *prompt != "" || flag.NArg() > 0 || sc.Prompt("", 0) != "":
		text := strings.TrimSpace(*prompt + " " + strings.Join(flag.Args(), " "))
		if text == "" {
			text = sc.Prompt("", sc.Duration-clock())
		}
		// A lead on a scenario's clock stops at its end, as the scripted
		// lead does; whatever it started is left as it is.
		runCtx := ctx
		if *scenarioPath != "" {
			var cancel context.CancelFunc
			runCtx, cancel = context.WithDeadline(ctx, t0.Add(sc.Duration))
			defer cancel()
		}
		turns.root = runCtx
		err = turns.run(func(tc context.Context) error { return a.Run(tc, text) })
		if errors.Is(err, context.DeadlineExceeded) || (runCtx.Err() == context.DeadlineExceeded && errors.Is(err, context.Canceled)) {
			printer.Line("time is up")
			err = nil
		}
	default:
		// The line-mode terminal: one line in, the agent's turn out. Lines
		// are read on their own goroutine so that a Ctrl-C at the prompt,
		// which ends the agent, is not held up by a read that never returns.
		lines := make(chan string)
		go func() {
			in := bufio.NewScanner(os.Stdin)
			for in.Scan() {
				lines <- in.Text()
			}
			close(lines)
		}()
		for err == nil {
			if adapter != nil {
				adapter.AwaitingInput(ctx)
			}
			fmt.Print("> ")
			var line string
			var ok bool
			select {
			case <-ctx.Done():
				fmt.Println()
				err = ctx.Err()
			case line, ok = <-lines:
			}
			if err != nil || !ok {
				break
			}
			if line = strings.TrimSpace(line); line != "" {
				err = turns.run(func(tc context.Context) error { return a.Run(tc, line) })
				switch {
				case errors.Is(err, errInterrupted):
					printer.Line("interrupted; press Ctrl-C again to quit")
					err = nil
				case real && errors.Is(err, llm.ErrQuota):
					printer.Line("the provider's usage limit was reached; this turn was not finished. Try again later, or start mini-agent with another --provider")
					err = nil
				}
			}
		}
	}
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
	case errors.Is(err, llm.ErrContextOverflow):
		// No compaction: the worker gives up and says why, and its planner
		// decides whether to split the task.
		printer.Line("giving up: " + err.Error())
		report(agent.Result{Task: workName, Status: "failed", Detail: "context_overflow"})
		return exitFailed
	case errors.Is(err, context.Canceled), errors.Is(err, errInterrupted):
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
func wrap(sc *scenarios.Scenario, self, task string, depth int, reg agent.Registry, askMemory, askGrow int64) (int, error) {
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
		// At a terminal the lead gets one of its own, relayed to this one,
		// so its prompt and Ctrl-C work as if it ran here directly.
		if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
			args = append(args, "-it")
		}
		if m := sc.Envelopes["lead"]; m > 0 {
			args = append(args, "--memory", strconv.FormatInt(m, 10))
		}
		args = append(append(args, "--", self), os.Args[1:]...)
		return 0, syscall.Exec(subactl, args, env)
	}
	if task == "" {
		return 0, errors.New("inside a lease a mini-agent needs --task")
	}
	// A scripted task is "kind:package". A real brain's task is free text,
	// and its children are all workers.
	kind, _, scripted := strings.Cut(task, ":")
	if !scripted || strings.ContainsAny(kind, " \t") {
		kind = "worker"
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
		// A planner that names the envelope itself has the last word.
		if askMemory > 0 {
			memory = askMemory
		}
		if memory == 0 {
			memory = 1 << 30
		}
		args := []string{"spawn", "--wait", "--kind", kind, "--memory", strconv.FormatInt(memory, 10)}
		switch {
		case askGrow > memory:
			args = append(args, "--grow-to", strconv.FormatInt(askGrow, 10))
		case askMemory == 0 && sc.Adaptive && sc.GrowFactor > 1:
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
	want := fmt.Sprintf("%s.d%d", labelOf(task), depth)
	for _, r := range reg.Results() {
		if r.Task == want && r.Status == "ok" {
			return 0, nil
		}
	}
	return exitFailed, nil
}

// labelOf names a task in logs and files. A scripted task is its own
// label; free text from a real brain is shortened to a few words and a
// hash, so that it is unique and safe in a file name.
func labelOf(task string) string {
	if len(task) <= 40 && !strings.ContainsAny(task, " \n\t/") {
		return task
	}
	h := fnv.New32a()
	h.Write([]byte(task))
	words := strings.Fields(task)
	if len(words) > 3 {
		words = words[:3]
	}
	return agent.TaskFile(strings.Join(words, "-")) + fmt.Sprintf("-%08x", h.Sum32())
}

// systemPrompt is all the model is told about its harness.
const systemPrompt = `You are mini-agent, a coding agent working in a shell on the user's machine.

You have four tools: read, write, edit and bash. Use bash for everything else: running tests, searching, git.

To delegate a self-contained piece of work, start a child agent from bash:

    mini-agent --task "what the child should do, with every detail it needs"

The call blocks until the child is done and exits 0 if it succeeded. The child shares nothing with you but the file system, so describe the task fully and tell it where to write its results. Run several children at once with "&" and "wait" when the pieces are independent.

When this runs under subactl, every agent has a hard memory limit and its children's limits come out of its own. "subactl available" shows what you can still hand out; "subactl ls" shows your children. If a child ends with code 413 it needed more memory than it was given: give the next attempt more, or a smaller task. Stop what you start: dev servers and browsers left running count against your limit.

If you receive a message marked "[subactl control message ...] TERMINATE_REQUESTED", save your progress, end your children and stop.

Be brief. When the task is done, say what you did and what is left.`

// errInterrupted is what a turn returns when Ctrl-C cut it short.
var errInterrupted = errors.New("interrupted")

// turnControl runs one turn at a time under a context of its own, so that
// Ctrl-C can cancel the turn without ending the agent.
type turnControl struct {
	root context.Context
	quit func()

	mu     sync.Mutex
	cancel context.CancelFunc // of the turn in progress, or nil
}

// run runs one turn. A turn cancelled by interrupt returns errInterrupted.
func (t *turnControl) run(turn func(context.Context) error) error {
	ctx, cancel := context.WithCancel(t.root)
	t.mu.Lock()
	t.cancel = cancel
	t.mu.Unlock()
	err := turn(ctx)
	t.mu.Lock()
	interrupted := t.cancel == nil // interrupt cleared it
	t.cancel = nil
	t.mu.Unlock()
	cancel()
	if interrupted && t.root.Err() == nil {
		return errInterrupted
	}
	return err
}

// interrupt cancels the turn in progress and reports true, or reports
// false when there is none, which means the agent should end.
func (t *turnControl) interrupt() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel == nil {
		return false
	}
	t.cancel()
	t.cancel = nil
	return true
}
