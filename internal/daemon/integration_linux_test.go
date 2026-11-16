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
	t    *testing.T
	m    *Manager
	be   *cgroup2.Backend
	base string
	uid  int
	op   Caller
	stop context.CancelFunc
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("integration tests need root")
	}
	uid := agentUID(t)
	prepareRoot(t)
	e := &env{t: t, uid: uid, base: fmt.Sprintf("/sys/fs/cgroup/subactl-test-%d-%d", os.Getpid(), time.Now().UnixNano()%1e6)}
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
	m, err := NewManager(ctx, Config{Backend: be, Capacity: 4 << 30, EscapeScan: 500 * time.Millisecond, TermWait: time.Second})
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
	grandkid := e.run(node(kid), 64*mib, "head -c 20000000 /dev/zero | tail & exec sleep 600")
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

	e.stop() // the daemon "crashes": nothing is told, nothing is cleaned up
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
