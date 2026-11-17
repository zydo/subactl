// Package darwinmon is the macOS backend. macOS has nothing like a cgroup,
// so this backend can only watch and react: it keeps a table of which
// processes belong to which acgroup, samples them, and kills what is over
// its limit after the fact. Every capability it reports is soft or
// monitor_only, and escape prevention is unsupported. Nothing here is a
// kernel guarantee.
//
// Membership: a lease's main process starts in its own process group. A
// process belongs to an acgroup if its parent does, or if it is in the
// process group of a member. The table is refreshed from the process
// table; a process that both leaves its group and outlives its parent
// between two scans is lost.
package darwinmon

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/internal/backend"
	"github.com/zydo/subactl/internal/pty"
)

// Config configures the backend.
type Config struct {
	// StateDir holds scratch directories and the membership file that lets
	// a restarted daemon find what the previous one left running.
	StateDir string
	Socket   string
}

type group struct {
	lease  api.LeaseID
	parent api.LeaseID
	env    api.Envelope
	// pids are the known member processes, pgids their process groups.
	pids  map[int]bool
	pgids map[int]bool
	peak  int64
}

func (g *group) Ref() backend.ACGroupRef { return backend.ACGroupRef{Lease: g.lease} }

// Backend implements backend.Backend by monitoring.
type Backend struct {
	cfg Config

	mu     sync.Mutex
	groups map[api.LeaseID]*group
	// left is what the membership file said a previous daemon left behind.
	left map[api.LeaseID][]int
	// scanned is when the process table was last read.
	scanned time.Time
}

// New prepares the state directory and reads the previous membership file.
func New(cfg Config) (*Backend, error) {
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "scratch"), 0o755); err != nil {
		return nil, fmt.Errorf("darwinmon: %w", err)
	}
	b := &Backend{cfg: cfg, groups: map[api.LeaseID]*group{}, left: map[api.LeaseID][]int{}}
	if data, err := os.ReadFile(b.membersFile()); err == nil {
		_ = json.Unmarshal(data, &b.left)
	}
	return b, nil
}

func (b *Backend) membersFile() string { return filepath.Join(b.cfg.StateDir, "darwin-members.json") }

func (b *Backend) Name() string           { return "darwin-monitor" }
func (b *Backend) MemBasis() api.MemBasis { return api.BasisPhysFootprint }

// Capabilities are reported as they are: nothing is enforced by the kernel.
func (b *Backend) Capabilities() api.Capabilities {
	return api.Capabilities{
		Membership: api.LevelSoft, Memory: api.LevelMonitorOnly, CPU: api.LevelMonitorOnly,
		Pids: api.LevelMonitorOnly, KillTree: api.LevelSoft, ParentDeath: api.LevelSoft,
		EscapePrevention: api.LevelUnsupported, CallerIdentity: api.LevelSoft,
	}
}

type procInfo struct {
	pid, ppid, pgid int
}

// processes reads the whole process table.
func processes() ([]procInfo, error) {
	list, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("darwinmon: read the process table: %w", err)
	}
	out := make([]procInfo, 0, len(list))
	for _, p := range list {
		out = append(out, procInfo{pid: int(p.Proc.P_pid), ppid: int(p.Eproc.Ppid), pgid: int(p.Eproc.Pgid)})
	}
	return out, nil
}

// scanLocked refreshes every acgroup's members from the process table:
// dead processes leave, and processes whose parent or process group
// belongs to an acgroup join it. It repeats until nothing changes, so that
// a chain of new descendants is found in one call.
func (b *Backend) scanLocked() error {
	procs, err := processes()
	if err != nil {
		return err
	}
	b.scanned = time.Now()
	alive := map[int]procInfo{}
	for _, p := range procs {
		alive[p.pid] = p
	}
	owner := map[int]*group{}
	byPgid := map[int]*group{}
	for _, g := range b.groups {
		for pid := range g.pids {
			if _, ok := alive[pid]; !ok {
				delete(g.pids, pid)
				continue
			}
			owner[pid] = g
		}
		for pgid := range g.pgids {
			byPgid[pgid] = g
		}
	}
	for changed := true; changed; {
		changed = false
		for _, p := range procs {
			if owner[p.pid] != nil || p.pid <= 1 {
				continue
			}
			g := owner[p.ppid]
			if g == nil {
				g = byPgid[p.pgid]
			}
			if g == nil {
				continue
			}
			g.pids[p.pid], owner[p.pid], changed = true, g, true
			// A member that made its own process group brings it along.
			if byPgid[p.pgid] == nil {
				g.pgids[p.pgid], byPgid[p.pgid] = true, g
			}
		}
	}
	b.saveLocked()
	return nil
}

// saveLocked writes the members to disk, for the next daemon's cleanup.
func (b *Backend) saveLocked() {
	out := map[api.LeaseID][]int{}
	for id, g := range b.groups {
		for pid := range g.pids {
			out[id] = append(out[id], pid)
		}
	}
	if data, err := json.Marshal(out); err == nil {
		_ = os.WriteFile(b.membersFile(), data, 0o600)
	}
}

func (b *Backend) group(g backend.ACGroup) (*group, error) {
	if own, ok := b.groups[g.Ref().Lease]; ok {
		return own, nil
	}
	return nil, fmt.Errorf("darwinmon: no acgroup for lease %s", g.Ref().Lease)
}

// Create registers the acgroup. There is no kernel object behind it.
func (b *Backend) Create(_ context.Context, lease api.LeaseID, parent backend.ACGroupRef, env api.Envelope) (backend.ACGroup, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, dup := b.groups[lease]; dup {
		return nil, fmt.Errorf("darwinmon: acgroup %s already exists", lease)
	}
	if parent.Lease != "" && b.groups[parent.Lease] == nil {
		return nil, fmt.Errorf("darwinmon: parent acgroup %s does not exist", parent.Lease)
	}
	g := &group{lease: lease, parent: parent.Lease, env: env, pids: map[int]bool{}, pgids: map[int]bool{}}
	b.groups[lease] = g
	return g, nil
}

// ScratchDir is where a lease's output files and results live.
func (b *Backend) ScratchDir(lease api.LeaseID) string {
	return filepath.Join(b.cfg.StateDir, "scratch", string(lease))
}

// RemoveScratch deletes a lease's scratch directory.
func (b *Backend) RemoveScratch(lease api.LeaseID) error {
	if lease == "" || filepath.Base(string(lease)) != string(lease) {
		return fmt.Errorf("darwinmon: invalid lease id %q", lease)
	}
	return os.RemoveAll(b.ScratchDir(lease))
}

// OutputBytes is the size of the lease's stdout and stderr files.
func (b *Backend) OutputBytes(_ context.Context, g backend.ACGroup) (int64, error) {
	var n int64
	for _, name := range []string{"stdout", "stderr"} {
		if info, err := os.Stat(filepath.Join(b.ScratchDir(g.Ref().Lease), name)); err == nil {
			n += info.Size()
		}
	}
	return n, nil
}

type proc struct {
	cmd    *exec.Cmd
	tty    *os.File
	once   sync.Once
	status backend.ExitStatus
}

func (p *proc) Pid() int      { return p.cmd.Process.Pid }
func (p *proc) TTY() *os.File { return p.tty }

func (p *proc) Wait() backend.ExitStatus {
	p.once.Do(func() {
		_ = p.cmd.Wait()
		if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			p.status = backend.ExitStatus{Signal: int(ws.Signal())}
			return
		}
		p.status = backend.ExitStatus{Code: p.cmd.ProcessState.ExitCode()}
	})
	return p.status
}

func (p *proc) Signal(sig syscall.Signal) error { return p.cmd.Process.Signal(sig) }

// Spawn starts the main process in a new session, which is also a new
// process group, and records it as the acgroup's first member. Unlike on
// Linux there is no moment before which the process is already confined:
// it is simply watched from here on.
func (b *Backend) Spawn(_ context.Context, g backend.ACGroup, c backend.Cmd) (backend.Proc, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	own, err := b.group(g)
	if err != nil {
		return nil, err
	}
	if c.UID == 0 {
		return nil, errors.New("darwinmon: an agent never runs as root")
	}
	scratch := b.ScratchDir(own.lease)
	if err := os.MkdirAll(scratch, 0o750); err != nil {
		return nil, err
	}
	cmd := exec.Command(c.Argv[0], c.Argv[1:]...)
	cmd.Dir = c.Dir
	cmd.Env = append(append([]string{}, c.Env...),
		"SUBACTL_LEASE_ID="+string(own.lease), "SUBACTL_SOCK="+b.cfg.Socket, "SUBACTL_SCRATCH="+scratch)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// A daemon running as root starts the agent as its user; a daemon
	// running as an ordinary user can only start agents as itself.
	if os.Geteuid() == 0 {
		_ = os.Chown(scratch, c.UID, -1)
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(c.UID), Gid: uint32(c.UID)}
	} else if c.UID != os.Getuid() {
		return nil, fmt.Errorf("darwinmon: this daemon runs as uid %d and cannot start an agent as uid %d", os.Getuid(), c.UID)
	}
	var master *os.File
	if c.TTY {
		pair, err := pty.Open()
		if err != nil {
			return nil, fmt.Errorf("darwinmon: %w", err)
		}
		slave, err := os.OpenFile(pair.Slave, os.O_RDWR|unix.O_NOCTTY, 0)
		if err != nil {
			pair.Master.Close()
			return nil, fmt.Errorf("darwinmon: pty slave: %w", err)
		}
		defer slave.Close()
		master = pair.Master
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		cmd.SysProcAttr.Setctty = true
	} else {
		open := func(name string) (*os.File, error) {
			return os.OpenFile(filepath.Join(scratch, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		}
		stdout, err := open("stdout")
		if err != nil {
			return nil, err
		}
		defer stdout.Close()
		stderr, err := open("stderr")
		if err != nil {
			return nil, err
		}
		defer stderr.Close()
		cmd.Stdout, cmd.Stderr = stdout, stderr
	}
	if err := cmd.Start(); err != nil {
		if master != nil {
			master.Close()
		}
		return nil, fmt.Errorf("darwinmon: spawn in %s: %w", own.lease, err)
	}
	pid := cmd.Process.Pid
	own.pids[pid], own.pgids[pid] = true, true // Setsid: the pid is also its process group
	b.saveLocked()
	return &proc{cmd: cmd, tty: master}, nil
}

// machToSeconds converts the kernel's CPU-time unit to seconds. On Apple
// silicon one unit is 125/3 nanoseconds; on Intel it is one nanosecond.
func machToSeconds(t uint64) float64 {
	if goruntime.GOARCH == "arm64" {
		return float64(t) * 125 / 3 / 1e9
	}
	return float64(t) / 1e9
}

// rusage reads a process's physical footprint and CPU time through the
// proc_info system call, which needs no C library binding.
func rusage(pid int) (footprint int64, cpuSeconds float64, ok bool) {
	// struct rusage_info_v0: uuid[16], user_time, system_time,
	// pkg_idle_wkups, interrupt_wkups, pageins, wired_size, resident_size,
	// phys_footprint, proc_start_abstime, proc_exit_abstime.
	var buf [96]byte
	const callPidRusage, flavorV0 = 9, 0
	if _, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, callPidRusage, uintptr(pid), flavorV0, 0, uintptr(unsafe.Pointer(&buf[0])), 0); errno != 0 {
		return 0, 0, false
	}
	user, system := binary.LittleEndian.Uint64(buf[16:]), binary.LittleEndian.Uint64(buf[24:])
	return int64(binary.LittleEndian.Uint64(buf[72:])), machToSeconds(user + system), true
}

// subtreeLocked lists the group and every group nested in it.
func (b *Backend) subtreeLocked(root *group) []*group {
	out := []*group{root}
	for i := 0; i < len(out); i++ {
		for _, g := range b.groups {
			if g.parent == out[i].lease {
				out = append(out, g)
			}
		}
	}
	return out
}

// Sample sums the physical footprint of the acgroup's processes, nested
// acgroups included, after refreshing the membership table.
func (b *Backend) Sample(_ context.Context, g backend.ACGroup) (api.Usage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	own, err := b.group(g)
	if err != nil {
		return api.Usage{}, err
	}
	// Samples of every lease come in a burst, several times a second: one
	// reading of the process table serves them all.
	if time.Since(b.scanned) > 100*time.Millisecond {
		if err := b.scanLocked(); err != nil {
			return api.Usage{}, err
		}
	}
	u := api.Usage{MemBasis: api.BasisPhysFootprint, SampledAt: time.Now()}
	for _, sub := range b.subtreeLocked(own) {
		for pid := range sub.pids {
			if mem, cpu, ok := rusage(pid); ok {
				u.Memory += mem
				u.CPUSeconds += cpu
			}
			u.Pids++
		}
	}
	own.peak = max(own.peak, u.Memory)
	u.MemoryPeak = own.peak
	return u, nil
}

// SetLimits records the envelope. The daemon compares samples against it.
func (b *Backend) SetLimits(_ context.Context, g backend.ACGroup, env api.Envelope) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	own, err := b.group(g)
	if err != nil {
		return err
	}
	own.env = env
	return nil
}

// KillTree kills every known member of the acgroup and of those nested in
// it, rescans for processes that appeared meanwhile, and repeats until a
// scan finds none. It reports what is left if that never happens.
func (b *Backend) KillTree(ctx context.Context, g backend.ACGroup) (backend.KillReport, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	own, err := b.group(g)
	if err != nil {
		return backend.KillReport{}, err
	}
	var rep backend.KillReport
	killed := map[int]bool{}
	for round := 0; round < 200; round++ {
		if err := b.scanLocked(); err != nil {
			return rep, err
		}
		left := 0
		for _, sub := range b.subtreeLocked(own) {
			for pgid := range sub.pgids {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
			for pid := range sub.pids {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				killed[pid] = true
				left++
			}
		}
		rep.Killed, rep.Remaining = len(killed), left
		if left == 0 {
			rep.Remaining = 0
			return rep, nil
		}
		if ctx.Err() != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rep.Killed -= rep.Remaining
	return rep, nil
}

// Destroy forgets the acgroup.
func (b *Backend) Destroy(_ context.Context, g backend.ACGroup) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	lease := g.Ref().Lease
	for _, other := range b.groups {
		if other.parent == lease {
			return fmt.Errorf("darwinmon: acgroup %s still has nested acgroups", lease)
		}
	}
	delete(b.groups, lease)
	delete(b.left, lease)
	b.saveLocked()
	return nil
}

// Identify finds the acgroup a process belongs to: by the table, or by
// walking up its parents to a known member. A process whose chain of
// parents is broken is taken for an operator, which is why this identity
// is only soft.
func (b *Backend) Identify(_ context.Context, pid int) (backend.ACGroupRef, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.scanLocked(); err != nil {
		return backend.ACGroupRef{}, err
	}
	for _, g := range b.groups {
		if g.pids[pid] {
			return g.Ref(), nil
		}
	}
	return backend.ACGroupRef{}, backend.ErrNotMember
}

// List reports what a previous daemon left running, from its membership
// file. The processes are adopted into placeholder acgroups so that the
// fail-closed cleanup can kill them.
func (b *Backend) List(context.Context) ([]backend.ACGroupRef, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []backend.ACGroupRef
	for lease, pids := range b.left {
		g := &group{lease: lease, pids: map[int]bool{}, pgids: map[int]bool{}}
		for _, pid := range pids {
			g.pids[pid] = true
		}
		b.groups[lease] = g
		out = append(out, g.Ref())
	}
	b.left = map[api.LeaseID][]int{}
	return out, nil
}

// WatchHolder reports the exit of a process through kqueue.
func (b *Backend) WatchHolder(ctx context.Context, pid int) (<-chan struct{}, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("darwinmon: kqueue: %w", err)
	}
	ev := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
		unix.Close(kq)
		return nil, fmt.Errorf("darwinmon: watch pid %d: %w", pid, err)
	}
	gone := make(chan struct{})
	go func() {
		defer unix.Close(kq)
		events := make([]unix.Kevent_t, 1)
		for ctx.Err() == nil {
			n, err := unix.Kevent(kq, nil, events, &unix.Timespec{Nsec: 500e6})
			if err != nil && !errors.Is(err, unix.EINTR) {
				return
			}
			if n > 0 {
				close(gone)
				return
			}
		}
	}()
	return gone, nil
}

// TopProcs lists the members of the acgroup and of those nested in it,
// largest physical footprint first, at most n.
func (b *Backend) TopProcs(_ context.Context, g backend.ACGroup, n int) ([]api.Proc, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	own, err := b.group(g)
	if err != nil {
		return nil, err
	}
	var out []api.Proc
	for _, sub := range b.subtreeLocked(own) {
		for pid := range sub.pids {
			mem, _, ok := rusage(pid)
			if !ok {
				continue
			}
			name := ""
			if kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid); err == nil {
				name = unix.ByteSliceToString(kp.Proc.P_comm[:])
			}
			out = append(out, api.Proc{Pid: pid, Name: name, Memory: mem})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Memory > out[j].Memory })
	return out[:min(n, len(out))], nil
}
