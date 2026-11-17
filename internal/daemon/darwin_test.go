//go:build darwin

// Tests of the lease lifecycle on the macOS monitoring backend, with real
// processes. They run as the ordinary user: the backend needs no root.
package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/internal/backend/darwinmon"
	"github.com/zydo/subactl/internal/pty"
)

const dmib = int64(1) << 20

type denv struct {
	t     *testing.T
	m     *Manager
	be    *darwinmon.Backend
	state string
	op    Caller
	stop  context.CancelFunc
}

func newDarwinEnv(t *testing.T) *denv {
	t.Helper()
	// A short path: scratch paths go into result validation and env.
	state, err := os.MkdirTemp("/tmp", "subactl-darwin-")
	if err != nil {
		t.Fatal(err)
	}
	e := &denv{t: t, state: state, op: Caller{UID: os.Getuid()}}
	e.start()
	t.Cleanup(func() {
		e.stop()
		e.m.Shutdown()
		os.RemoveAll(state)
	})
	return e
}

// start brings up a backend and manager on the same state directory;
// calling it again models a daemon restart.
func (e *denv) start() {
	e.t.Helper()
	be, err := darwinmon.New(darwinmon.Config{StateDir: e.state, Socket: "/nonexistent"})
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m, err := NewManager(ctx, Config{Backend: be, Capacity: 8 << 30, Tick: 200 * time.Millisecond, TermWait: time.Second})
	if err != nil {
		e.t.Fatal(err)
	}
	e.be, e.m, e.stop = be, m, cancel
}

func (e *denv) run(c Caller, envl api.Envelope, script string) api.LeaseID {
	e.t.Helper()
	ls, err := e.m.Acquire(context.Background(), c, api.AcquireRequest{Kind: "worker", Envelope: envl})
	if err != nil {
		e.t.Fatalf("acquire: %v", err)
	}
	_, err = e.m.Spawn(context.Background(), c, ls.ID, api.SpawnRequest{Argv: []string{"/bin/sh", "-c", script}, Env: []string{"PATH=/usr/bin:/bin:/usr/sbin"}, Dir: "/"})
	if err != nil {
		e.t.Fatalf("spawn: %v", err)
	}
	return ls.ID
}

func (e *denv) wait(id api.LeaseID, within time.Duration) api.Termination {
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

// sleeping counts the sleep processes with a marker duration.
func sleeping(marker string) int {
	out, _ := exec.Command("pgrep", "-f", "^sleep "+marker+"$").Output()
	return len(strings.Fields(string(out)))
}

func soon(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", within, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// F13: the backend says what it is. Nothing is reported as hard, and a
// request that requires hard memory enforcement is refused, not weakened.
func TestDarwinHonestLevels(t *testing.T) {
	e := newDarwinEnv(t)
	c := e.be.Capabilities()
	for name, level := range map[string]api.Level{"membership": c.Membership, "memory": c.Memory, "cpu": c.CPU, "pids": c.Pids, "kill_tree": c.KillTree, "parent_death": c.ParentDeath, "identity": c.CallerIdentity} {
		if level == api.LevelHard {
			t.Errorf("%s is reported as hard on a backend that only monitors", name)
		}
	}
	if c.Memory != api.LevelMonitorOnly || c.EscapePrevention != api.LevelUnsupported || e.be.MemBasis() != api.BasisPhysFootprint {
		t.Fatalf("capabilities %+v, basis %s", c, e.be.MemBasis())
	}
	_, err := e.m.Acquire(context.Background(), e.op, api.AcquireRequest{Envelope: api.Envelope{Memory: 64 * dmib}, Require: &api.Require{Memory: api.LevelHard}})
	wantStatus(t, err, 409, api.ErrRejected, api.ReasonEnforcementUnavailable)
	if _, err := e.m.Acquire(context.Background(), e.op, api.AcquireRequest{Envelope: api.Envelope{Memory: 64 * dmib}, Require: &api.Require{Memory: api.LevelMonitorOnly}}); err != nil {
		t.Fatal(err)
	}
}

// Descendants that leave the process group and outlive their parent are
// still found and killed with the lease, as long as a scan saw them.
func TestDarwinReapsDescendants(t *testing.T) {
	e := newDarwinEnv(t)
	// nohup in a subshell: new process group via the shell's job control
	// is not available in sh -c, so these detach by double fork.
	id := e.run(e.op, api.Envelope{Memory: 256 * dmib}, "(nohup sleep 31401 >/dev/null 2>&1 &); (nohup sleep 31401 >/dev/null 2>&1 &); sleep 31401 & wait")
	soon(t, 5*time.Second, "three descendants run and are known as members", func() bool {
		ls, err := e.m.Get(context.Background(), e.op, id)
		return sleeping("31401") == 3 && err == nil && ls.Usage != nil && ls.Usage.Pids >= 4
	})
	main := e.m.runtime(id).proc.Pid()
	if err := syscall.Kill(main, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	term := e.wait(id, 5*time.Second)
	if term.Code != api.CodeExternalAbort || term.Enforcement != api.LevelMonitorOnly {
		t.Fatalf("%+v", term)
	}
	soon(t, 2*time.Second, "no descendant is left", func() bool { return sleeping("31401") == 0 })
}

// Memory is limited by sampling: a lease over its limit is killed as a
// whole with 413, and the record says the enforcement was monitor_only.
// A neighbour is not touched, and a nested lease's use counts in its parent.
func TestDarwinMemoryLimitBySampling(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("uses python3 to hold a known amount of memory")
	}
	hold := func(mb int) string {
		return fmt.Sprintf(`exec python3 -c 'import time
b = bytearray(%d * 1024 * 1024)
for i in range(0, len(b), 4096): b[i] = 1
time.sleep(600)'`, mb)
	}
	e := newDarwinEnv(t)
	bystander := e.run(e.op, api.Envelope{Memory: 64 * dmib}, "exec sleep 600")
	lead := e.run(e.op, api.Envelope{Memory: 1024 * dmib}, "exec sleep 600")
	kid := e.run(node(lead), api.Envelope{Memory: 512 * dmib}, hold(150))
	soon(t, 10*time.Second, "the child's memory shows in the child and in its parent", func() bool {
		for _, id := range []api.LeaseID{kid, lead} {
			ls, err := e.m.Get(context.Background(), e.op, id)
			if err != nil || ls.Usage == nil || ls.Usage.Memory < 140*dmib || ls.Usage.MemBasis != api.BasisPhysFootprint {
				return false
			}
		}
		return true
	})
	leaker := e.run(e.op, api.Envelope{Memory: 100 * dmib}, "(nohup sleep 31402 >/dev/null 2>&1 &); "+hold(300))
	term := e.wait(leaker, 15*time.Second)
	if term.Code != api.CodeOOMLeaseLimit || term.Dimension != "memory" || term.Enforcement != api.LevelMonitorOnly || term.Evidence["detected_by"] != "sampling" {
		t.Fatalf("%+v", term)
	}
	soon(t, 2*time.Second, "the leaker's descendant is gone", func() bool { return sleeping("31402") == 0 })
	for _, id := range []api.LeaseID{bystander, lead, kid} {
		if ls, _ := e.m.Get(context.Background(), e.op, id); ls.State != api.StateRunning {
			t.Fatalf("%s was affected: %+v", id, ls)
		}
	}
}

// The holder of a lead dies; the tree goes with it, as 502.
func TestDarwinHolderDeath(t *testing.T) {
	e := newDarwinEnv(t)
	holder := exec.Command("sleep", "600")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer holder.Process.Kill()
	op := Caller{UID: os.Getuid(), PID: holder.Process.Pid}
	lead := e.run(op, api.Envelope{Memory: 256 * dmib}, "(nohup sleep 31403 >/dev/null 2>&1 &); exec sleep 600")
	kid := e.run(node(lead), api.Envelope{Memory: 64 * dmib}, "(nohup sleep 31403 >/dev/null 2>&1 &); exec sleep 600")
	soon(t, 5*time.Second, "the tree is running", func() bool { return sleeping("31403") == 2 })
	_ = holder.Process.Kill()
	_ = holder.Wait()
	for _, id := range []api.LeaseID{lead, kid} {
		if term := e.wait(id, 5*time.Second); term.Code != api.CodeParentDied {
			t.Fatalf("%s: %+v", id, term)
		}
	}
	soon(t, 2*time.Second, "nothing of the tree is left", func() bool { return sleeping("31403") == 0 })
}

// A process inside a lease is identified as that lease, also when it is a
// grandchild; a process outside every lease is an operator.
func TestDarwinIdentity(t *testing.T) {
	e := newDarwinEnv(t)
	id := e.run(e.op, api.Envelope{Memory: 128 * dmib}, "sh -c 'sleep 31404' & wait")
	var grandchild int
	soon(t, 5*time.Second, "the grandchild runs", func() bool {
		out, _ := exec.Command("pgrep", "-f", "^sleep 31404$").Output()
		fmt.Sscan(string(out), &grandchild)
		return grandchild > 0
	})
	ref, err := e.be.Identify(context.Background(), grandchild)
	if err != nil || ref.Lease != id {
		t.Fatalf("grandchild identified as %v, %v", ref, err)
	}
	if _, err := e.be.Identify(context.Background(), os.Getpid()); err == nil {
		t.Fatal("the test process was identified as a lease member")
	}
}

// Fail-closed: a daemon that vanishes leaves processes running; the next
// one finds them through the membership file and kills them all.
func TestDarwinFailClosed(t *testing.T) {
	e := newDarwinEnv(t)
	lead := e.run(e.op, api.Envelope{Memory: 256 * dmib}, "(nohup sleep 31405 >/dev/null 2>&1 &); exec sleep 31405")
	e.run(node(lead), api.Envelope{Memory: 64 * dmib}, "exec sleep 31405")
	soon(t, 5*time.Second, "the old tree runs and its members are recorded", func() bool {
		ls, err := e.m.Get(context.Background(), e.op, lead)
		return sleeping("31405") == 3 && err == nil && ls.Usage != nil && ls.Usage.Pids >= 3
	})
	e.m.abandoned.Store(true) // the daemon is gone without a word
	e.stop()
	if n := sleeping("31405"); n != 3 {
		t.Fatalf("%d processes after the crash, want the 3 still running", n)
	}
	e.start()
	soon(t, 5*time.Second, "the new daemon killed what the old one left", func() bool { return sleeping("31405") == 0 })
	if h := e.m.Host(); h.Allocated != 0 {
		t.Fatalf("the new daemon adopted %d bytes", h.Allocated)
	}
}

// A lead with a terminal, on macOS.
func TestDarwinLeadTerminal(t *testing.T) {
	e := newDarwinEnv(t)
	ls, err := e.m.Acquire(context.Background(), e.op, api.AcquireRequest{TTY: true, Envelope: api.Envelope{Memory: 64 * dmib}})
	if err != nil {
		t.Fatal(err)
	}
	script := `test -t 0 && echo is-a-tty; read line; stty size; echo "typed:$line"`
	if _, err := e.m.Spawn(context.Background(), e.op, ls.ID, api.SpawnRequest{Argv: []string{"/bin/sh", "-c", script}, Env: []string{"PATH=/usr/bin:/bin"}, Dir: "/"}); err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	go e.m.Attach(e.op, ls.ID, server)
	var out strings.Builder
	read := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := client.Read(buf)
			out.Write(buf[:n])
			if err != nil {
				close(read)
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	pty.WriteFrame(client, pty.FrameResize, pty.ResizePayload(37, 111))
	time.Sleep(200 * time.Millisecond)
	pty.WriteFrame(client, pty.FrameData, []byte("hello mac\n"))
	if term := e.wait(ls.ID, 10*time.Second); term.Code != api.CodeCompleted {
		t.Fatalf("%+v", term)
	}
	server.Close()
	<-read
	got := strings.ReplaceAll(out.String(), "\r", "")
	for _, want := range []string{"is-a-tty\n", "37 111\n", "typed:hello mac\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("the terminal output lacks %q:\n%s", want, got)
		}
	}
	_ = filepath.Join
}

// The macOS backend lists a lease's processes by footprint, and a lease
// ended for its memory names them in its evidence.
func TestDarwinTopProcs(t *testing.T) {
	e := newDarwinEnv(t)
	id := e.run(e.op, api.Envelope{Memory: 1 << 30}, "exec sleep 31")
	var top []api.Proc
	soon(t, 5*time.Second, "the sleep is listed", func() bool {
		top, _ = e.be.TopProcs(context.Background(), e.m.runtime(id).group, 3)
		return len(top) == 1 && top[0].Name == "sleep" // after the shell's exec
	})
	if top[0].Memory <= 0 {
		t.Fatalf("top processes %+v", top)
	}
	go e.m.Kill(context.Background(), e.op, id)
	e.wait(id, 5*time.Second)

	// Over its limit: the sampler ends it, and names the process.
	id = e.run(e.op, api.Envelope{Memory: 48 * dmib}, `exec perl -e '$x = "x" x (200*1024*1024); sleep 30'`)
	term := e.wait(id, 15*time.Second)
	got, _ := term.Evidence["top_procs"].([]api.Proc)
	if term.Code != api.CodeOOMLeaseLimit || len(got) == 0 || got[0].Name != "perl" {
		t.Fatalf("ended with %d, evidence %+v", term.Code, term.Evidence)
	}
}
