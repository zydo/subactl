//go:build linux && integration

// Integration tests of the lease lifecycle on the real cgroup v2 backend.
// They need root and a writable cgroup v2 hierarchy:
//
//	go test -c -tags integration -o daemon.test ./internal/daemon
//	sudo ./daemon.test -test.run Integration -test.v
//
// Each test is named after the acceptance criterion it covers.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/internal/backend"
	"github.com/zydo/subactl/internal/backend/cgroup2"
	"github.com/zydo/subactl/internal/capacity"
	"github.com/zydo/subactl/internal/telemetry"
)

const mib = int64(1) << 20

// The test binary doubles as the pre-exec shim, as subactld does.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == cgroup2.ShimArg {
		fmt.Fprintln(os.Stderr, cgroup2.ShimMain(os.Args[2:]))
		os.Exit(126)
	}
	os.Exit(m.Run())
}

// agentUID is the unprivileged user the test agents run as.
func agentUID(t *testing.T) int {
	t.Helper()
	uid := 1000
	if s := os.Getenv("SUBACTL_TEST_UID"); s != "" {
		uid, _ = strconv.Atoi(s)
	}
	if _, err := user.LookupId(strconv.Itoa(uid)); err != nil {
		t.Skipf("no user with uid %d; set SUBACTL_TEST_UID", uid)
	}
	return uid
}

// prepareRoot lets the cgroup root delegate controllers. Inside a
// container the root is an ordinary cgroup that holds processes, which
// must move to a leaf first.
func prepareRoot(t *testing.T) {
	t.Helper()
	const root = "/sys/fs/cgroup"
	ctl := filepath.Join(root, "cgroup.subtree_control")
	if err := os.WriteFile(ctl, []byte("+memory +pids"), 0o644); err == nil {
		return
	} else if !errors.Is(err, syscall.EBUSY) {
		t.Skipf("cannot delegate cgroup controllers: %v", err)
	}
	leaf := filepath.Join(root, "test-init")
	_ = os.MkdirAll(leaf, 0o755)
	data, _ := os.ReadFile(filepath.Join(root, "cgroup.procs"))
	for _, pid := range strings.Fields(string(data)) {
		_ = os.WriteFile(filepath.Join(leaf, "cgroup.procs"), []byte(pid), 0o644)
	}
	if err := os.WriteFile(ctl, []byte("+memory +pids"), 0o644); err != nil {
		t.Skipf("cannot delegate cgroup controllers: %v", err)
	}
}

type env struct {
	// outer, when set, is a capped cgroup that stands in for the machine:
	// the leases live under it and capacity is read from it.
	outer   string
	reserve int64
	window  time.Duration

	t    *testing.T
	m    *Manager
	be   *cgroup2.Backend
	base string
	uid  int
	op   Caller
	stop context.CancelFunc
}

func newEnv(t *testing.T) *env { return newEnvIn(t, 0, 0) }

// newEnvIn builds the environment inside an outer cgroup capped at total
// bytes, with the given reserve line, when total is not zero.
func newEnvIn(t *testing.T, total, reserve int64) *env {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("integration tests need root")
	}
	uid := agentUID(t)
	prepareRoot(t)
	e := &env{t: t, uid: uid, base: fmt.Sprintf("/sys/fs/cgroup/subactl-test-%d-%d", os.Getpid(), time.Now().UnixNano()%1e6)}
	if total > 0 {
		e.outer, e.reserve, e.window = e.base, reserve, 2*time.Second
		e.base = filepath.Join(e.outer, "leases")
		for _, d := range []string{e.outer, filepath.Join(e.outer, "external")} {
			if err := os.Mkdir(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for name, v := range map[string]string{"memory.max": strconv.FormatInt(total, 10), "memory.swap.max": "0", "cgroup.subtree_control": "+memory +pids"} {
			if err := os.WriteFile(filepath.Join(e.outer, name), []byte(v), 0o644); err != nil && name != "memory.swap.max" {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			_ = os.WriteFile(filepath.Join(e.outer, "cgroup.kill"), []byte("1"), 0o644)
			time.Sleep(100 * time.Millisecond)
			_ = os.Remove(filepath.Join(e.outer, "external"))
			_ = os.Remove(e.outer)
		})
	}
	e.op = Caller{UID: uid}
	e.start()
	t.Cleanup(func() {
		e.stop()
		e.m.Shutdown()
		_ = os.Remove(e.base)
	})
	return e
}

// start brings up a backend and a manager on the same base cgroup. Calling
// it again models a daemon restart.
func (e *env) start() {
	e.t.Helper()
	self, _ := os.Executable()
	// Not t.TempDir: its parent is private to root, and agents must be able
	// to reach their scratch directories.
	state, err := os.MkdirTemp("/tmp", "subactl-state-")
	if err != nil {
		e.t.Fatal(err)
	}
	_ = os.Chmod(state, 0o755)
	e.t.Cleanup(func() { os.RemoveAll(state) })
	be, err := cgroup2.New(cgroup2.Config{Base: e.base, StateDir: state, Socket: "/nonexistent", Shim: self})
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cfg := Config{Backend: be, Capacity: 4 << 30, EscapeScan: 500 * time.Millisecond, TermWait: time.Second}
	if e.outer != "" {
		cfg.Capacity, cfg.Host, cfg.Reserve, cfg.SafetyWindow = 0, capacity.CgroupReader{Dir: e.outer}, e.reserve, e.window
	}
	m, err := NewManager(ctx, cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	e.be, e.m, e.stop = be, m, cancel
}

// run acquires a lease and starts sh -c script in it.
func (e *env) run(c Caller, mem int64, script string) api.LeaseID {
	e.t.Helper()
	ls, err := e.m.Acquire(context.Background(), c, api.AcquireRequest{Kind: "worker", Envelope: api.Envelope{Memory: mem}})
	if err != nil {
		e.t.Fatalf("acquire: %v", err)
	}
	_, err = e.m.Spawn(context.Background(), c, ls.ID, api.SpawnRequest{Argv: []string{"/bin/sh", "-c", script}, Env: []string{"PATH=/usr/local/bin:/usr/bin:/bin"}, Dir: "/"})
	if err != nil {
		e.t.Fatalf("spawn: %v", err)
	}
	return ls.ID
}

func (e *env) wait(id api.LeaseID, within time.Duration) api.Termination {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	began := time.Now()
	term, ok := e.m.waitReaped(ctx, id)
	if !ok {
		e.t.Fatalf("lease %s was not reaped within %v", id, within)
	}
	e.t.Logf("lease %s: %d %s after %v", id, term.Code, term.Name, time.Since(began).Round(time.Millisecond))
	return term
}

// pidsWith lists the processes on the host whose command line starts with
// s. A prefix, so that a shell whose script merely mentions s is not counted.
func pidsWith(s string) []int {
	var out []int
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		cmd, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if strings.HasPrefix(strings.ReplaceAll(string(cmd), "\x00", " "), s) {
			out = append(out, pid)
		}
	}
	return out
}

func eventually(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", within, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// hog allocates memory without end: tail buffers a stream with no newline.
const hog = "exec tail /dev/zero"

// F14: nested leases; the parent's own processes sit in the _self leaf, so
// creating a nested lease never fails with EBUSY; a child's usage counts
// against its parent.
func TestIntegrationF14Nesting(t *testing.T) {
	e := newEnv(t)
	lead := e.run(e.op, 512*mib, "exec sleep 600")
	kid := e.run(node(lead), 128*mib, "exec sleep 600")
	grandkid := e.run(node(kid), 64*mib, "x=$(head -c 20000000 /dev/zero | tr '\\0' x); sleep 600") // the shell holds about 20MB
	for _, p := range []string{string(lead), filepath.Join(string(lead), string(kid)), filepath.Join(string(lead), string(kid), string(grandkid))} {
		procs, err := os.ReadFile(filepath.Join(e.base, p, "_self", "cgroup.procs"))
		if err != nil || len(strings.Fields(string(procs))) == 0 {
			t.Fatalf("no process in %s/_self: %v", p, err)
		}
		if direct, _ := os.ReadFile(filepath.Join(e.base, p, "cgroup.procs")); len(strings.Fields(string(direct))) != 0 {
			t.Fatalf("processes sit directly in %s", p)
		}
	}
	eventually(t, 5*time.Second, "the grandchild's memory shows up in every ancestor", func() bool {
		for _, id := range []api.LeaseID{grandkid, kid, lead} {
			ls, err := e.m.Get(context.Background(), e.op, id)
			if err != nil || ls.Usage == nil || ls.Usage.Memory < 15*mib || ls.Usage.MemBasis != api.BasisWorkingSet {
				return false
			}
		}
		return true
	})
	// Ending the middle lease takes its subtree and nothing else.
	if _, err := e.m.Kill(context.Background(), e.op, kid); err != nil {
		t.Fatal(err)
	}
	if term := e.wait(grandkid, 5*time.Second); term.Code != api.CodeParentDied {
		t.Fatalf("grandkid: %+v", term)
	}
	if ls, _ := e.m.Get(context.Background(), e.op, lead); ls.State != api.StateRunning {
		t.Fatalf("lead: %+v", ls)
	}
	if _, err := os.Stat(filepath.Join(e.base, string(lead), string(kid))); !os.IsNotExist(err) {
		t.Fatalf("cgroup of a reaped lease still exists: %v", err)
	}
}

// F33: every process of a lease runs as the requested uid, never as root.
func TestIntegrationF33RunsAsUser(t *testing.T) {
	e := newEnv(t)
	id := e.run(e.op, 64*mib, "id -u > $SUBACTL_SCRATCH/uid; cat /proc/self/oom_score_adj > $SUBACTL_SCRATCH/adj; echo -1000 > /proc/self/oom_score_adj; echo $? > $SUBACTL_SCRATCH/lower")
	if term := e.wait(id, 5*time.Second); term.Code != api.CodeCompleted {
		t.Fatalf("%+v", term)
	}
	// The scratch directory is removed with the lease in this milestone, so
	// run a second lease that keeps running while its files are read.
	id = e.run(e.op, 64*mib, "id -u > $SUBACTL_SCRATCH/uid; cat /proc/self/oom_score_adj > $SUBACTL_SCRATCH/adj; (echo -1000 > /proc/self/oom_score_adj) 2>/dev/null; echo $? > $SUBACTL_SCRATCH/lower; exec sleep 600")
	read := func(name string) string {
		var out string
		eventually(t, 5*time.Second, "the agent wrote "+name, func() bool {
			data, err := os.ReadFile(filepath.Join(e.be.ScratchDir(id), name))
			out = strings.TrimSpace(string(data))
			return err == nil && out != ""
		})
		return out
	}
	if got := read("uid"); got != strconv.Itoa(e.uid) {
		t.Fatalf("agent runs as uid %s, want %d", got, e.uid)
	}
	if got := read("adj"); got != "200" {
		t.Fatalf("oom_score_adj is %s", got)
	}
	// The value set by the shim is a floor the agent cannot go below.
	if got := read("lower"); got == "0" {
		t.Fatal("the agent lowered its own oom_score_adj")
	}
	if _, err := e.m.Acquire(context.Background(), Caller{UID: 0, Admin: true}, api.AcquireRequest{Envelope: api.Envelope{Memory: mib}}); err == nil {
		t.Fatal("root got a lead without naming a non-root uid")
	}
}

// F2: an external SIGKILL of the main process ends the lease with 520 and
// takes every descendant with it; a main process that exits cleanly but
// leaves descendants ends with 200, and they are reaped and counted.
func TestIntegrationF2MainExit(t *testing.T) {
	e := newEnv(t)
	// setsid and nohup: descendants that a process-group kill would miss.
	id := e.run(e.op, 128*mib, "setsid nohup sleep 31337.1 >/dev/null 2>&1 & setsid nohup sleep 31337.1 >/dev/null 2>&1 & sleep 31337.1 & wait")
	eventually(t, 5*time.Second, "three descendants are running", func() bool { return len(pidsWith("sleep 31337.1")) == 3 })
	main := e.m.runtime(id).proc.Pid()
	if err := syscall.Kill(main, syscall.SIGKILL); err != nil { // not through subactld
		t.Fatal(err)
	}
	term := e.wait(id, 5*time.Second)
	if term.Code != api.CodeExternalAbort || term.Evidence["signal"] != int(syscall.SIGKILL) || term.Evidence["reaped_pids"] != 3 {
		t.Fatalf("%+v", term)
	}
	if left := pidsWith("sleep 31337.1"); len(left) != 0 {
		t.Fatalf("descendants survived: %v", left)
	}

	id = e.run(e.op, 128*mib, "setsid nohup sleep 31337.2 >/dev/null 2>&1 & sleep 0.3; exit 0")
	term = e.wait(id, 5*time.Second)
	if term.Code != api.CodeCompleted || term.Evidence["reaped_pids"] != 1 {
		t.Fatalf("%+v", term)
	}
	if left := pidsWith("sleep 31337.2"); len(left) != 0 {
		t.Fatalf("descendants survived a clean exit: %v", left)
	}
}

// F1 and F15: the holder of a lead is SIGKILLed; three leases, each with
// stand-ins for a browser, are gone within 5 seconds with 502, and no
// process carrying the lease marker is left on the host.
func TestIntegrationF1HolderDeath(t *testing.T) {
	e := newEnv(t)
	holder := exec.Command("sleep", "600") // stands in for "subactl run"
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer holder.Process.Kill()
	op := Caller{UID: e.uid, PID: holder.Process.Pid}
	chrome := "for i in 1 2 3 4; do setsid nohup sleep 31337.3 >/dev/null 2>&1 & done; exec sleep 600"
	lead := e.run(op, 512*mib, chrome)
	kids := []api.LeaseID{e.run(node(lead), 128*mib, chrome), e.run(node(lead), 128*mib, chrome)}
	eventually(t, 5*time.Second, "twelve browser stand-ins are running", func() bool { return len(pidsWith("sleep 31337.3")) == 12 })

	began := time.Now()
	_ = holder.Process.Kill()
	_ = holder.Wait()
	if term := e.wait(lead, 5*time.Second); term.Code != api.CodeParentDied {
		t.Fatalf("lead: %+v", term)
	}
	for _, k := range kids {
		if term := e.wait(k, 5*time.Second); term.Code != api.CodeParentDied {
			t.Fatalf("%s: %+v", k, term)
		}
	}
	if left := pidsWith("sleep 31337.3"); len(left) != 0 {
		t.Fatalf("%d processes survived the holder: %v", len(left), left)
	}
	if found, _ := e.be.ScanEscapes(context.Background()); len(found) != 0 {
		t.Fatalf("processes with a lease marker remain: %v", found)
	}
	t.Logf("tree reaped %v after the holder died", time.Since(began).Round(time.Millisecond))
}

// F4 and F14: a lease that leaks to its hard limit is the only one that
// dies, with 413; when a parent's own processes hit the parent's limit, the
// parent ends with 413 and its nested lease with 502.
func TestIntegrationF4MemoryIsolation(t *testing.T) {
	e := newEnv(t)
	bystander := e.run(e.op, 64*mib, "exec sleep 600")
	leaker := e.run(e.op, 64*mib, hog)
	term := e.wait(leaker, 30*time.Second)
	if term.Code != api.CodeOOMLeaseLimit || term.Dimension != "memory" || term.Limit != 64*mib || term.Enforcement != api.LevelHard {
		t.Fatalf("leaker: %+v", term)
	}
	if term.Peak < 60*mib || term.Peak > 64*mib {
		t.Errorf("peak %d is not just under the 64MiB limit", term.Peak)
	}
	if ls, _ := e.m.Get(context.Background(), e.op, bystander); ls.State != api.StateRunning {
		t.Fatalf("the bystander was affected: %+v", ls)
	}

	parent := e.run(e.op, 96*mib, "sleep 1; "+hog)
	child := e.run(node(parent), 32*mib, "exec sleep 600")
	if term := e.wait(parent, 30*time.Second); term.Code != api.CodeOOMLeaseLimit {
		t.Fatalf("parent: %+v", term)
	}
	if term := e.wait(child, 5*time.Second); term.Code != api.CodeParentDied {
		t.Fatalf("child: %+v", term)
	}
	// A nested lease that hits its own limit dies alone.
	parent = e.run(e.op, 256*mib, "exec sleep 600")
	child = e.run(node(parent), 32*mib, hog)
	if term := e.wait(child, 30*time.Second); term.Code != api.CodeOOMLeaseLimit {
		t.Fatalf("child: %+v", term)
	}
	if ls, _ := e.m.Get(context.Background(), e.op, parent); ls.State != api.StateRunning {
		t.Fatalf("the parent died with its child: %+v", ls)
	}
}

// F3: a process cannot move itself out of its acgroup through cgroupfs; a
// process that is outside the lease tree but still carries the marker is
// found and killed within 5 seconds, and its lease ends with 451.
func TestIntegrationF3Escape(t *testing.T) {
	e := newEnv(t)
	// Try the cgroup root and, by its real path, the test's own base.
	script := fmt.Sprintf(`
(echo $$ > /sys/fs/cgroup/cgroup.procs) 2>/dev/null; echo "root=$?" >> $SUBACTL_SCRATCH/out
(echo $$ > %s/cgroup.procs) 2>/dev/null; echo "base=$?" >> $SUBACTL_SCRATCH/out
cat /proc/self/cgroup >> $SUBACTL_SCRATCH/out
echo done >> $SUBACTL_SCRATCH/out
exec sleep 600`, e.base)
	id := e.run(e.op, 64*mib, script)
	var out string
	eventually(t, 5*time.Second, "the escape attempts finished", func() bool {
		data, _ := os.ReadFile(filepath.Join(e.be.ScratchDir(id), "out"))
		out = string(data)
		return strings.Contains(out, "done")
	})
	if strings.Contains(out, "root=0") || strings.Contains(out, "base=0") {
		t.Fatalf("a migration out of the acgroup succeeded:\n%s", out)
	}
	// Inside its cgroup namespace the agent sees its own leaf as the root.
	if !strings.Contains(out, "0::/\n") {
		t.Errorf("the agent sees a cgroup path beyond its namespace root:\n%s", out)
	}
	main := e.m.runtime(id).proc.Pid()
	if ref, err := e.be.Identify(context.Background(), main); err != nil || ref.Lease != id {
		t.Fatalf("the agent is no longer in its acgroup: %v %v", ref, err)
	}

	// Now move a process out as root would, which the agent cannot do, to
	// exercise the scan: it keeps its environment, so it is found.
	if err := os.WriteFile("/sys/fs/cgroup/test-init/cgroup.procs", []byte(strconv.Itoa(main)), 0o644); err != nil {
		if err = os.WriteFile("/sys/fs/cgroup/cgroup.procs", []byte(strconv.Itoa(main)), 0o644); err != nil {
			t.Fatalf("cannot stage an escape: %v", err)
		}
	}
	term := e.wait(id, 5*time.Second)
	if term.Code != api.CodeEscapeDetected || term.Evidence["escaped_pid"] != main {
		t.Fatalf("%+v", term)
	}
	eventually(t, 2*time.Second, "the escaped process is dead", func() bool { return syscall.Kill(main, 0) != nil })
}

// F12: the daemon is lost without warning. While it is down the limits
// still hold; a new daemon kills everything from before within 5 seconds
// and adopts nothing.
func TestIntegrationF12FailClosed(t *testing.T) {
	e := newEnv(t)
	lead := e.run(e.op, 256*mib, "setsid nohup sleep 31337.4 >/dev/null 2>&1 & exec sleep 31337.4")
	kid := e.run(node(lead), 48*mib, "sleep 2; "+hog)
	eventually(t, 5*time.Second, "the old tree is running", func() bool { return len(pidsWith("sleep 31337.4")) == 2 })
	kidDir := filepath.Join(e.base, string(lead), string(kid))

	// The daemon "crashes": nothing is told, nothing is cleaned up, and
	// from here on it reacts to nothing.
	e.m.abandoned.Store(true)
	e.stop()
	// With no daemon, the kernel still enforces the kid's 48MiB limit.
	eventually(t, 30*time.Second, "the kernel enforced the limit while the daemon was down", func() bool {
		data, _ := os.ReadFile(filepath.Join(kidDir, "memory.events"))
		return strings.Contains(string(data), "oom_kill ") && !strings.Contains(string(data), "oom_kill 0")
	})
	if peak, _ := os.ReadFile(filepath.Join(kidDir, "memory.peak")); len(peak) > 0 {
		if n, _ := strconv.ParseInt(strings.TrimSpace(string(peak)), 10, 64); n > 48*mib {
			t.Fatalf("usage reached %d, above the 48MiB limit, while the daemon was down", n)
		}
	}

	began := time.Now()
	e.start() // a new daemon on the same base
	if left := pidsWith("sleep 31337.4"); len(left) != 0 {
		t.Fatalf("processes from before the restart survived: %v", left)
	}
	if entries, _ := os.ReadDir(e.base); countDirs(entries) != 0 {
		t.Fatalf("cgroups from before the restart remain: %v", entries)
	}
	if h := e.m.Host(); h.Allocated != 0 {
		t.Fatalf("the new daemon adopted %d bytes", h.Allocated)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("cleanup took %v", took)
	}
	// And it works: a fresh lease runs.
	id := e.run(e.op, 64*mib, "exit 0")
	if term := e.wait(id, 5*time.Second); term.Code != api.CodeCompleted {
		t.Fatalf("%+v", term)
	}
}

func countDirs(entries []os.DirEntry) int {
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return n
}

// F13: the backend reports what it really reaches, and a requirement it
// cannot meet is refused rather than quietly weakened.
func TestIntegrationF13HonestLevels(t *testing.T) {
	e := newEnv(t)
	caps := e.be.Capabilities()
	if caps.Memory != api.LevelHard || caps.KillTree != api.LevelHard || caps.Pids != api.LevelHard {
		t.Fatalf("capabilities %+v", caps)
	}
	req := api.AcquireRequest{Envelope: api.Envelope{Memory: 32 * mib}, Require: &api.Require{Memory: api.LevelHard, CPU: api.LevelHard}}
	_, err := e.m.Acquire(context.Background(), e.op, req)
	if caps.CPU == api.LevelHard {
		if err != nil {
			t.Fatal(err)
		}
	} else {
		wantStatus(t, err, 409, api.ErrRejected, api.ReasonEnforcementUnavailable)
	}
	// The pids limit is real: a fork bomb is refused at the limit. The
	// forking happens in a subshell, which may die of it; the main process
	// then carries on without forking.
	id := e.m.mustRun(t, e.op, api.Envelope{Memory: 64 * mib, Pids: 8},
		"(for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16; do sleep 600 & done) 2>/dev/null; exec sleep 600")
	dir := filepath.Join(e.base, string(id))
	eventually(t, 5*time.Second, "forks were refused at the pids limit", func() bool {
		data, _ := os.ReadFile(filepath.Join(dir, "pids.events"))
		return strings.Contains(string(data), "max ") && !strings.Contains(string(data), "max 0")
	})
	if peak, _ := os.ReadFile(filepath.Join(dir, "pids.peak")); strings.TrimSpace(string(peak)) != "8" {
		t.Fatalf("pids.peak is %q, want exactly the limit of 8", strings.TrimSpace(string(peak)))
	}
	if ls, err := e.m.Get(context.Background(), e.op, id); err != nil || ls.State != api.StateRunning {
		t.Fatalf("the lease did not survive the fork bomb: %+v, %v", ls, err)
	}
	var _ backend.Backend = e.be
}

func (m *Manager) mustRun(t *testing.T, c Caller, envl api.Envelope, script string) api.LeaseID {
	t.Helper()
	ls, err := m.Acquire(context.Background(), c, api.AcquireRequest{Envelope: envl})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Spawn(context.Background(), c, ls.ID, api.SpawnRequest{Argv: []string{"/bin/sh", "-c", script}, Env: []string{"PATH=/usr/local/bin:/usr/bin:/bin"}, Dir: "/"}); err != nil {
		t.Fatal(err)
	}
	return ls.ID
}

// F11 and the 408 half of F10: a lease that writes past its output limit
// is killed as a whole with 414, having written no more than the limit
// plus about one tick of output; a lease past its wall time ends with 408.
func TestIntegrationF11OutputAndWall(t *testing.T) {
	e := newEnv(t)
	const limit = 4 * mib
	// About 1MiB per 100ms, so one 1s tick is roughly 10MiB of overrun.
	chatty := e.m.mustRun(t, e.op, api.Envelope{Memory: 64 * mib, Output: limit},
		"setsid nohup sleep 31337.5 >/dev/null 2>&1 & while :; do head -c 1048576 /dev/zero | tr '\\0' x; sleep 0.1; done")
	out := filepath.Join(e.be.ScratchDir(chatty), "stdout")
	var size int64
	done := make(chan api.Termination, 1)
	go func() { done <- e.wait(chatty, 15*time.Second) }()
	var term api.Termination
	for term.Code == 0 {
		select {
		case term = <-done:
		default:
			if info, err := os.Stat(out); err == nil {
				size = info.Size() // the file is removed with the lease; keep the last size seen
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if term.Code != api.CodeOutputExceeded || term.Dimension != "output" {
		t.Fatalf("%+v", term)
	}
	if size <= limit || size > limit+16*mib {
		t.Fatalf("the lease wrote %d bytes against a limit of %d; want the limit plus at most about one second of output", size, limit)
	}
	if left := pidsWith("sleep 31337.5"); len(left) != 0 {
		t.Fatalf("descendants survived the output kill: %v", left)
	}

	began := time.Now()
	slow := e.m.mustRun(t, e.op, api.Envelope{Memory: 64 * mib, Wall: 2}, "exec sleep 600")
	if term := e.wait(slow, 10*time.Second); term.Code != api.CodeWallExceeded || term.Dimension != "wall" {
		t.Fatalf("%+v", term)
	}
	if took := time.Since(began); took < 2*time.Second || took > 4*time.Second {
		t.Fatalf("the wall limit of 2s fired after %v", took)
	}
}

// hold is a process that allocates n bytes at once, touches them and keeps
// them. It notes the time of the allocation in its scratch directory.
func hold(n int64) string {
	return fmt.Sprintf(`exec node -e 'const b=Buffer.alloc(%d,1); require("fs").writeFileSync(process.env.SUBACTL_SCRATCH+"/held", String(Date.now())); setInterval(()=>b[0],1000)'`, n)
}

func needNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("this test uses node to hold a known amount of memory")
	}
}

// rssOf sums the resident memory of the processes in a cgroup subtree, as
// the kernel reports it per process: an independent view of the same use.
func rssOf(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.Name() != "cgroup.procs" {
			return nil
		}
		data, _ := os.ReadFile(path)
		for _, pid := range strings.Fields(string(data)) {
			status, _ := os.ReadFile("/proc/" + pid + "/status")
			for _, line := range strings.Split(string(status), "\n") {
				if f := strings.Fields(line); len(f) >= 2 && f[0] == "VmRSS:" {
					kb, _ := strconv.ParseInt(f[1], 10, 64)
					total += kb << 10
				}
			}
		}
		return nil
	})
	return total
}

// F9: usage agrees with the kernel's own per-process accounting to within
// 10%, and a sample is never older than two seconds.
func TestIntegrationF9Accuracy(t *testing.T) {
	needNode(t)
	e := newEnv(t)
	id := e.run(e.op, 512*mib, hold(256*mib))
	dir := filepath.Join(e.base, string(id))
	eventually(t, 10*time.Second, "the process holds its memory", func() bool { return rssOf(t, dir) > 256*mib })
	time.Sleep(1500 * time.Millisecond) // let a sample be taken after the allocation
	tr, err := e.m.Trace(e.op, id)
	if err != nil || len(tr.Samples) == 0 {
		t.Fatalf("trace: %+v, %v", tr, err)
	}
	last := tr.Samples[len(tr.Samples)-1]
	if age := time.Since(last.SampledAt); age > 2*time.Second {
		t.Fatalf("the latest sample is %v old", age)
	}
	rss := rssOf(t, dir)
	diff := float64(last.Memory-rss) / float64(rss)
	t.Logf("working_set %d, sum of VmRSS %d, difference %+.1f%%", last.Memory, rss, 100*diff)
	if last.MemBasis != api.BasisWorkingSet || diff > 0.10 || diff < -0.10 {
		t.Fatalf("usage %d differs from the kernel's %d by %.1f%%", last.Memory, rss, 100*diff)
	}
}

// F7: NEAR_LIMIT arrives within two seconds of usage passing 90% of the
// hard limit, and nothing is terminated for it.
func TestIntegrationF7NearLimit(t *testing.T) {
	needNode(t)
	e := newEnv(t)
	events, stop := e.m.Events(e.op)
	defer stop()
	// 262MiB plus about 14MiB for the runtime itself, against 300MiB:
	// above 90%, below 100%.
	id := e.run(e.op, 300*mib, "sleep 1; "+hold(262*mib))
	var held time.Time
	eventually(t, 10*time.Second, "the process allocated", func() bool {
		data, err := os.ReadFile(filepath.Join(e.be.ScratchDir(id), "held"))
		ms, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		held = time.UnixMilli(ms)
		return err == nil && ms > 0
	})
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type != telemetry.EventNearLimit {
				continue
			}
			delay := ev.At.Sub(held)
			t.Logf("NEAR_LIMIT %v after the allocation: %v", delay.Round(time.Millisecond), ev.Data)
			if ev.Lease != id || delay > 2*time.Second {
				t.Fatalf("NEAR_LIMIT for %s came %v after the allocation", ev.Lease, delay)
			}
			if ls, _ := e.m.Get(context.Background(), e.op, id); ls.State != api.StateRunning {
				t.Fatalf("the lease was terminated: %+v", ls)
			}
			return
		case <-deadline:
			t.Fatal("no NEAR_LIMIT within 5s of the allocation")
		}
	}
}

// F8 on real memory. A 640MiB cgroup stands in for the machine, with a
// 160MiB reserve line. An external process grows until available memory is
// below the line. Nothing happens to the leases while only the capacity
// has shrunk; once memory is really short, the planner is warned, and
// after the window exactly one lease, the largest leaf, is ended with 503.
func TestIntegrationF8SafetyNet(t *testing.T) {
	needNode(t)
	e := newEnvIn(t, 640*mib, 160*mib)
	events, stop := e.m.Events(e.op)
	defer stop()
	lead := e.run(e.op, 400*mib, "exec sleep 600")
	small := e.run(node(lead), 140*mib, hold(40*mib))
	large := e.run(node(lead), 240*mib, hold(130*mib))
	time.Sleep(3 * time.Second)
	if h := e.m.Host(); h.MemTotal != 640*mib || h.EffectiveCapacity < 400*mib {
		t.Fatalf("host %+v", h)
	}

	// The external load: a process in the outer cgroup, outside every lease.
	// The leases use about 200MiB; with 310MiB more, about 130MiB is left.
	external := exec.Command("/bin/sh", "-c", hold(296*mib))
	external.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "SUBACTL_SCRATCH=/tmp"}
	leaf, err := os.Open(filepath.Join(e.outer, "external"))
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	external.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(leaf.Fd())}
	began := time.Now()
	if err := external.Start(); err != nil {
		t.Fatal(err)
	}
	defer external.Process.Kill()

	var warned time.Time
	var ended []api.Event
	deadline := time.After(40 * time.Second)
	for done := false; !done; {
		select {
		case ev := <-events:
			switch {
			case ev.Type == telemetry.EventCapacityShrunk && ev.Data["reason"] == "below_reserve":
				warned = ev.At
				t.Logf("warned %v after the load began: %v", ev.At.Sub(began).Round(time.Millisecond), ev.Data)
			case ev.Type == telemetry.EventTerminated:
				ended = append(ended, ev)
				t.Logf("terminated %s (%v) %v after the warning", ev.Lease, ev.Data["code"], ev.At.Sub(warned).Round(time.Millisecond))
				// Watch a while longer: nothing more may be terminated.
				deadline = time.After(12 * time.Second)
			}
		case <-deadline:
			done = true
		}
	}
	if warned.IsZero() {
		t.Fatal("no warning that memory is below the reserve line")
	}
	if len(ended) != 1 || ended[0].Lease != large || ended[0].Data["code"] != float64(api.CodeSafetyNet) {
		t.Fatalf("want exactly the largest leaf (%s) ended with 503, got %+v", large, ended)
	}
	if d := ended[0].At.Sub(warned); d < e.window || d > e.window+3*time.Second {
		t.Fatalf("the termination came %v after the warning; the window is %v", d, e.window)
	}
	for _, id := range []api.LeaseID{lead, small} {
		if ls, _ := e.m.Get(context.Background(), e.op, id); ls.State != api.StateRunning {
			t.Fatalf("%s was ended too: %+v", id, ls)
		}
	}
	if h, _ := (capacity.CgroupReader{Dir: e.outer}).Read(); h.Available < 160*mib {
		t.Fatalf("available memory is still below the line: %d", h.Available)
	}
}
