package bench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zydo/subactl/examples/mini-agent/agent"
)

const cgroupRoot = "/sys/fs/cgroup"

// chamber is the outer cgroup of one run:
//
//	<base>/<name>/           memory.max = the chamber size
//	<base>/<name>/agents/    the lead and everything it starts
//	<base>/<name>/external/  the interference process, not an agent
//	<base>/<name>/probe/     the watchdog's liveness probe
type chamber struct {
	dir string
}

func write(path, value string) error {
	return os.WriteFile(path, []byte(value), 0o644)
}

// enable turns on the memory and pids controllers for the children of dir.
func enable(dir string) error {
	return write(filepath.Join(dir, "cgroup.subtree_control"), "+memory +pids")
}

// prepareRoot makes sure children of the cgroup root get the controllers.
// On a host the root cgroup may hold processes and still delegate. Inside a
// container the "root" is an ordinary cgroup, which may not delegate while
// it holds processes, so they are moved to a leaf first.
func prepareRoot() error {
	err := enable(cgroupRoot)
	if err == nil || !errors.Is(err, syscall.EBUSY) {
		return err
	}
	leaf := filepath.Join(cgroupRoot, "bench-init")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.procs"))
	if err != nil {
		return err
	}
	for _, pid := range strings.Fields(string(data)) {
		_ = write(filepath.Join(leaf, "cgroup.procs"), pid) // a pid may have exited
	}
	return enable(cgroupRoot)
}

func newChamber(base, name string, bytes int64) (*chamber, error) {
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		return nil, fmt.Errorf("cgroup v2 is not mounted at %s: %w", cgroupRoot, err)
	}
	if err := prepareRoot(); err != nil {
		return nil, fmt.Errorf("enable controllers at the cgroup root: %w", err)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	if err := enable(base); err != nil {
		return nil, err
	}
	c := &chamber{dir: filepath.Join(base, name)}
	if err := os.Mkdir(c.dir, 0o755); err != nil {
		return nil, err
	}
	if err := write(filepath.Join(c.dir, "memory.max"), strconv.FormatInt(bytes, 10)); err != nil {
		return nil, err
	}
	// Without swap the cap is a cap on real memory. Not every kernel has
	// the swap controller, so a failure here is not fatal.
	_ = write(filepath.Join(c.dir, "memory.swap.max"), "0")
	if err := enable(c.dir); err != nil {
		return nil, err
	}
	for _, leaf := range []string{"agents", "external", "probe", "holder"} {
		if err := os.Mkdir(filepath.Join(c.dir, leaf), 0o755); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// start runs cmd inside a leaf of the chamber from its first instruction.
func (c *chamber) start(leaf string, cmd *exec.Cmd) error {
	f, err := os.Open(filepath.Join(c.dir, leaf))
	if err != nil {
		return err
	}
	defer f.Close()
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(f.Fd())
	return cmd.Start()
}

func readInt(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return n
}

// stat reads one key of a flat-keyed cgroup file such as memory.stat.
func stat(path, key string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), " "); ok && k == key {
			n, _ := strconv.ParseInt(v, 10, 64)
			return n
		}
	}
	return 0
}

// workingSet is memory.current minus inactive file cache: the design's
// working_set basis.
func workingSet(dir string) int64 {
	return max(readInt(filepath.Join(dir, "memory.current"))-stat(filepath.Join(dir, "memory.stat"), "inactive_file"), 0)
}

func (c *chamber) sample(t time.Duration, agents string) Sample {
	return Sample{
		T:        t.Seconds(),
		WS:       workingSet(c.dir),
		AgentsWS: workingSet(filepath.Join(c.dir, agents)),
		Pids:     readInt(filepath.Join(c.dir, "pids.current")),
		OOMKills: stat(filepath.Join(c.dir, "memory.events"), "oom_kill"),
	}
}

// pids lists every process in a leaf and the cgroups below it.
func (c *chamber) pids(leaf string) []int {
	var out []int
	_ = filepath.WalkDir(filepath.Join(c.dir, leaf), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.Name() != "cgroup.procs" {
			return nil
		}
		data, _ := os.ReadFile(path)
		for _, f := range strings.Fields(string(data)) {
			if pid, err := strconv.Atoi(f); err == nil {
				out = append(out, pid)
			}
		}
		return nil
	})
	return out
}

// destroy kills everything in the chamber and removes it.
func (c *chamber) destroy() error {
	_ = write(filepath.Join(c.dir, "cgroup.kill"), "1")
	deadline := time.Now().Add(30 * time.Second)
	for readInt(filepath.Join(c.dir, "pids.current")) > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	// Remove child cgroups deepest first, then the chamber itself.
	var dirs []string
	_ = filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	var last error
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Remove(dirs[i]); err != nil {
			last = err
		}
	}
	return last
}

// terminationCodes counts, from subactld's audit log, the leases that
// ended since a given time, by code.
func terminationCodes(dir string, since time.Time) map[string]int {
	out := map[string]int{}
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			var r struct {
				At          time.Time `json:"at"`
				Type        string    `json:"type"`
				Termination *struct {
					Code int `json:"code"`
				} `json:"termination"`
			}
			if json.Unmarshal(sc.Bytes(), &r) == nil && r.Type == "terminate" && r.Termination != nil && !r.At.Before(since) {
				out[strconv.Itoa(r.Termination.Code)]++
			}
		}
		f.Close()
	}
	return out
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func cmdline(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(bytes.ReplaceAll(data, []byte{0}, []byte{' '})))
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// Run plays one scenario in a fresh chamber and returns its metrics. It
// must run as root, to create the chamber.
func Run(o Options) (*Summary, error) {
	sc := o.Scenario
	if err := os.MkdirAll(o.OutDir, 0o755); err != nil {
		return nil, err
	}
	runDir := filepath.Join(o.OutDir, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, err
	}
	// The lead runs as an ordinary user and must own its run directory.
	if err := os.Chown(runDir, o.UID, o.GID); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s-%d-%d", strings.ToLower(o.Group), os.Getpid(), o.RunIndex)
	ch, err := newChamber(o.Base, name, o.ChamberBytes)
	if err != nil {
		return nil, err
	}
	defer ch.destroy() // on early returns; a second call is harmless
	series, err := NewSeriesWriter(filepath.Join(o.OutDir, "series.jsonl"))
	if err != nil {
		return nil, err
	}
	defer series.Close()
	leadLog, err := os.Create(filepath.Join(o.OutDir, "lead.log"))
	if err != nil {
		return nil, err
	}
	defer leadLog.Close()

	cred := &syscall.Credential{Uid: uint32(o.UID), Gid: uint32(o.GID)}
	env := append(os.Environ(), "SUBACTL_EXPERIMENT="+name, "HOME="+o.Home)
	// Without subactl the lead and everything it starts live in one leaf.
	// With it, subactld owns a base cgroup inside the chamber and takes the
	// chamber for the machine; only the lead's holder sits outside the base.
	agents, leadLeaf := "agents", "agents"
	var stopDaemon func()
	if o.Mode == "with" {
		agents, leadLeaf = "subactl", "holder"
		sock := filepath.Join(o.OutDir, "subactl.sock")
		dlog, err := os.Create(filepath.Join(o.OutDir, "subactld.log"))
		if err != nil {
			return nil, err
		}
		defer dlog.Close()
		daemon := exec.Command(filepath.Join(o.BinDir, "subactld"),
			"--socket", sock, "--socket-group", o.SocketGroup,
			"--cgroup", filepath.Join(ch.dir, agents), "--capacity-cgroup", ch.dir,
			"--reserve", strconv.FormatInt(o.Reserve, 10), "--state", o.StateDir)
		daemon.Stdout, daemon.Stderr = dlog, dlog
		if err := daemon.Start(); err != nil {
			return nil, fmt.Errorf("start subactld: %w", err)
		}
		// Stopping the daemon reaps every lease; the chamber is removed after.
		stopDaemon = sync.OnceFunc(func() {
			_ = daemon.Process.Signal(syscall.SIGTERM)
			_ = daemon.Wait()
		})
		defer stopDaemon()
		for i := 0; ; i++ {
			if _, err := os.Stat(sock); err == nil {
				break
			}
			if i > 100 {
				return nil, fmt.Errorf("subactld did not come up; see %s", dlog.Name())
			}
			time.Sleep(100 * time.Millisecond)
		}
		adaptive := "0"
		if o.Adaptive {
			adaptive = "1"
		}
		env = append(env, "SUBACTL_SOCK="+sock, "MINI_AGENT_ADAPTIVE="+adaptive, "PATH="+o.BinDir+":"+os.Getenv("PATH"))
	}
	lead := exec.Command(o.MiniAgent, "--mode", o.Mode, "--scenario", o.ScenarioPath, "--run-dir", runDir)
	lead.Dir, lead.Env = o.WorkDir, env
	lead.Stdout, lead.Stderr = leadLog, leadLog
	lead.SysProcAttr = &syscall.SysProcAttr{Credential: cred, Setsid: true}
	start := time.Now()
	if err := ch.start(leadLeaf, lead); err != nil {
		return nil, fmt.Errorf("start the lead: %w", err)
	}
	o.Log("%s run %d: lead started (pid %d) in %s, capped at %.1fGiB", o.Group, o.RunIndex, lead.Process.Pid, ch.dir, float64(o.ChamberBytes)/gib)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	var meltdown atomic.Bool

	// Sampler: once a second, for the whole run including the settle time.
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			series.Add(ch.sample(time.Since(start), agents))
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()

	// Watchdog: a trivial process must be able to run inside the chamber.
	// If none has for 60 seconds, the chamber has melted down: kill it.
	wg.Add(1)
	go func() {
		defer wg.Done()
		lastOK := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
			probe := exec.CommandContext(pctx, "/bin/true")
			if err := ch.start("probe", probe); err == nil && probe.Wait() == nil {
				lastOK = time.Now()
			}
			pcancel()
			if ctx.Err() == nil && time.Since(lastOK) > 60*time.Second {
				meltdown.Store(true)
				o.Log("%s run %d: the chamber has not responded for 60s; killing it (meltdown)", o.Group, o.RunIndex)
				_ = write(filepath.Join(ch.dir, "cgroup.kill"), "1")
				return
			}
		}
	}()

	// External interference: a process that is not an agent takes a share
	// of the chamber for a while.
	if sc.InterferenceFor > 0 && sc.InterferenceShare > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
				return
			case <-time.After(sc.InterferenceAt):
			}
			bytes := int64(float64(o.ChamberBytes) * sc.InterferenceShare)
			hog := exec.CommandContext(ctx, o.Self, "hog", "--bytes", strconv.FormatInt(bytes, 10),
				"--ramp", sc.InterferenceRamp.String(), "--hold", sc.InterferenceFor.String())
			o.Log("%s run %d: external interference of %.1fGiB begins", o.Group, o.RunIndex, float64(bytes)/gib)
			if err := ch.start("external", hog); err == nil {
				_ = hog.Wait()
			}
		}()
	}

	// The lead ends itself at the scenario's duration. Twice that plus a
	// margin means it is stuck.
	done := make(chan error, 1)
	go func() { done <- lead.Wait() }()
	sum := &Summary{Group: o.Group, Run: o.RunIndex, Scenario: filepath.Base(o.ScenarioPath), ChamberBytes: o.ChamberBytes, MemBasis: "working_set"}
	select {
	case err := <-done:
		if err != nil {
			sum.LeadExit = 1
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				sum.LeadExit = ee.ExitCode()
			}
		}
	case <-time.After(2*sc.Duration + 5*time.Minute):
		meltdown.Store(true)
		_ = write(filepath.Join(ch.dir, "cgroup.kill"), "1")
		<-done
		sum.LeadExit = -1
	}
	sum.Makespan = time.Since(start).Minutes()
	o.Log("%s run %d: lead ended after %.1f min (exit %d); observing for %v", o.Group, o.RunIndex, sum.Makespan, sum.LeadExit, o.Settle)

	// What is still running after the settle time was left behind.
	time.Sleep(o.Settle)
	left := ch.pids(agents)
	sum.OrphansAfter = len(left)
	sum.LeakedWSAfter = float64(workingSet(filepath.Join(ch.dir, agents))) / gib
	for i, pid := range left {
		if i == 8 {
			break
		}
		sum.OrphanSample = append(sum.OrphanSample, cmdline(pid))
	}
	cancel()
	wg.Wait()
	sum.Meltdown = meltdown.Load()
	Summarize(sum, series.All)

	// Completion: packages whose upgrade reported ok, over all packages.
	list := exec.Command("bash", "-c", `"$TASK_DIR/list.sh" | head -n "${MAX_PACKAGES:-1000}"`)
	list.Dir = o.WorkDir
	list.Env = append(append(env, sc.TaskEnv("", 0)...), "TASK_DIR="+filepath.Join(runDir, "task"), "RUN_DIR="+runDir)
	if out, err := list.Output(); err == nil {
		sum.Packages = len(strings.Fields(string(out)))
	}
	ok := 0
	for _, r := range (agent.Registry{Dir: runDir}).Results() {
		if strings.HasPrefix(r.Task, "upgrade:") && r.Status == "ok" {
			ok++
		}
	}
	if sum.Packages > 0 {
		sum.Completion = float64(ok) / float64(sum.Packages)
	}
	// Work lost: subagents that were killed and whose task had to be redone.
	logs, _ := filepath.Glob(filepath.Join(runDir, "work", "*", "transcript.log"))
	for _, l := range logs {
		data, _ := os.ReadFile(l)
		sum.WorkLost += strings.Count(string(data), "usage limit: killed subagent")
	}

	// What subactld recorded: how each lease of this run ended. A lease
	// ended by its own limit (413) or by the safety net (503) is work that
	// has to be done again.
	if stopDaemon != nil {
		stopDaemon() // flushes the audit log
		sum.Codes = terminationCodes(filepath.Join(o.StateDir, "audit"), start)
		sum.SafetyNet = sum.Codes["503"]
		sum.WorkLost += sum.Codes["413"] + sum.Codes["503"]
	}

	// The task may have things to clean up outside the run directory. Its
	// processes must be gone first, or they keep writing there.
	if err := ch.destroy(); err != nil {
		o.Log("chamber %s was not fully removed: %v", ch.dir, err)
	}
	if teardown := filepath.Join(runDir, "task", "teardown.sh"); fileExists(teardown) {
		td := exec.Command(teardown)
		td.Env = append(list.Env, "RUN_ID="+name)
		td.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
		if out, err := td.CombinedOutput(); err != nil {
			o.Log("teardown failed: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}

	data, _ := json.MarshalIndent(sum, "", "  ")
	if err := os.WriteFile(filepath.Join(o.OutDir, "summary.json"), data, 0o644); err != nil {
		return nil, err
	}
	return sum, nil
}
