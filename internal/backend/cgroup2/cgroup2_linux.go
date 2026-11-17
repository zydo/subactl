// Package cgroup2 is the Linux enforcement backend: an acgroup is a cgroup
// v2 directory, membership is cgroup membership, and kill_tree is
// cgroup.kill. It needs Linux 5.14 or later and must run as root.
//
// Layout, under the base cgroup:
//
//	<lease>/             limits are set here
//	<lease>/_self/       the lease's own processes live in this leaf
//	<lease>/<child>/     a nested lease, with the same shape
//
// cgroup v2 forbids a cgroup that delegates controllers to its children
// from holding processes itself, hence the _self leaf.
package cgroup2

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/internal/backend"
	"github.com/zydo/subactl/internal/pty"
)

const (
	cgroupRoot = "/sys/fs/cgroup"
	selfLeaf   = "_self"
	// ShimArg is the hidden first argument that makes subactld act as the
	// pre-exec shim instead of the daemon.
	ShimArg = "__exec"
	// agentOOMScoreAdj is written by the shim while it is still privileged.
	// That also makes it the floor: an agent cannot lower it again. It must
	// stay above -1000, which would exempt the process from memory.oom.group.
	agentOOMScoreAdj = 200
)

// Config configures the backend.
type Config struct {
	// Base is the cgroup under which every lease lives, e.g.
	// /sys/fs/cgroup/subactl.
	Base string
	// StateDir holds per-lease scratch directories.
	StateDir string
	// Socket is exported to agents as SUBACTL_SOCK.
	Socket string
	// Shim is the binary re-executed as the pre-exec shim; normally the
	// daemon itself.
	Shim string
}

// Backend implements backend.Backend on cgroup v2.
type Backend struct {
	cfg  Config
	caps api.Capabilities

	mu      sync.Mutex
	dirs    map[api.LeaseID]string // lease -> its cgroup directory
	outputs map[api.LeaseID]*output
	limits  map[api.LeaseID]int64 // output limit, set before the output exists
}

type group struct {
	lease api.LeaseID
	dir   string
}

func (g *group) Ref() backend.ACGroupRef { return backend.ACGroupRef{Lease: g.lease} }

func write(path, value string) error { return os.WriteFile(path, []byte(value), 0o644) }

// New checks the host and prepares the base cgroup.
func New(cfg Config) (*Backend, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("cgroup2: the backend must run as root")
	}
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		return nil, fmt.Errorf("cgroup2: cgroup v2 is not mounted at %s (cgroup v1 is not supported): %w", cgroupRoot, err)
	}
	if rel, err := filepath.Rel(cgroupRoot, cfg.Base); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("cgroup2: base %q must be a directory below %s", cfg.Base, cgroupRoot)
	}
	if err := os.MkdirAll(cfg.Base, 0o755); err != nil {
		return nil, fmt.Errorf("cgroup2: %w", err)
	}
	// cgroup.kill arrived in Linux 5.14; without it kill_tree is not atomic.
	if _, err := os.Stat(filepath.Join(cfg.Base, "cgroup.kill")); err != nil {
		return nil, errors.New("cgroup2: cgroup.kill is missing; Linux 5.14 or later is required")
	}
	// Controllers must be delegated from the root down to the base. memory
	// and pids are required; cpu is reported honestly if it is missing.
	b := &Backend{cfg: cfg, dirs: map[api.LeaseID]string{}, outputs: map[api.LeaseID]*output{}, limits: map[api.LeaseID]int64{}}
	hard := api.LevelHard
	b.caps = api.Capabilities{
		Membership: hard, Memory: hard, CPU: hard, Pids: hard, KillTree: hard,
		ParentDeath: hard, EscapePrevention: hard, CallerIdentity: hard,
	}
	// Under a systemd service with Delegate=yes, the base is below the
	// daemon's own cgroup, which holds the daemon. A cgroup that holds
	// processes cannot hand controllers down, so the daemon first moves
	// itself into a leaf of its own.
	if err := moveSelfAside(cfg.Base); err != nil {
		return nil, err
	}
	// Delegation runs from the root down: a cgroup can only pass on what
	// its own parent passed to it.
	var chain []string
	for dir := filepath.Dir(cfg.Base); ; dir = filepath.Dir(dir) {
		chain = append([]string{dir}, chain...)
		if dir == cgroupRoot {
			break
		}
	}
	for _, dir := range chain {
		if err := write(filepath.Join(dir, "cgroup.subtree_control"), "+memory +pids"); err != nil {
			return nil, fmt.Errorf("cgroup2: cannot delegate memory and pids at %s: %w", dir, err)
		}
		if err := write(filepath.Join(dir, "cgroup.subtree_control"), "+cpu"); err != nil {
			b.caps.CPU = api.LevelUnsupported
		}
	}
	if err := b.delegate(cfg.Base); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "scratch"), 0o755); err != nil {
		return nil, fmt.Errorf("cgroup2: %w", err)
	}
	return b, nil
}

// moveSelfAside moves the daemon into <own cgroup>/_daemon when base lies
// below its own cgroup. It does nothing otherwise.
func moveSelfAside(base string) error {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return fmt.Errorf("cgroup2: %w", err)
	}
	rel, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "0::")
	if !ok || rel == "/" {
		return nil
	}
	own := filepath.Join(cgroupRoot, rel)
	if r, err := filepath.Rel(own, base); err != nil || r == "." || strings.HasPrefix(r, "..") {
		return nil
	}
	leaf := filepath.Join(own, "_daemon")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		return fmt.Errorf("cgroup2: %w", err)
	}
	// Every process of the service goes, not only this one: a process left
	// behind would keep the controllers from being handed down.
	procs, err := os.ReadFile(filepath.Join(own, "cgroup.procs"))
	if err != nil {
		return fmt.Errorf("cgroup2: %w", err)
	}
	for _, pid := range strings.Fields(string(procs)) {
		if err := write(filepath.Join(leaf, "cgroup.procs"), pid); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("cgroup2: move process %s out of %s: %w", pid, own, err)
		}
	}
	return nil
}

// delegate enables the controllers for the children of dir.
func (b *Backend) delegate(dir string) error {
	ctl := "+memory +pids"
	if b.caps.CPU == api.LevelHard {
		ctl += " +cpu"
	}
	if err := write(filepath.Join(dir, "cgroup.subtree_control"), ctl); err != nil {
		return fmt.Errorf("cgroup2: enable controllers in %s: %w", dir, err)
	}
	return nil
}

func (b *Backend) Name() string                   { return "cgroup2" }
func (b *Backend) MemBasis() api.MemBasis         { return api.BasisWorkingSet }
func (b *Backend) Capabilities() api.Capabilities { return b.caps }

// dir returns the cgroup directory of an acgroup.
func (b *Backend) dir(g backend.ACGroup) (string, error) {
	if cg, ok := g.(*group); ok {
		return cg.dir, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if d, ok := b.dirs[g.Ref().Lease]; ok {
		return d, nil
	}
	return "", fmt.Errorf("cgroup2: no acgroup for lease %s", g.Ref().Lease)
}

func validLease(id api.LeaseID) bool {
	s := string(id)
	return s != "" && s != selfLeaf && s != "." && s != ".." && !strings.ContainsAny(s, "/\x00")
}

func limits(dir string, env api.Envelope, cpu bool) error {
	orMax := func(n int64) string {
		if n <= 0 {
			return "max"
		}
		return strconv.FormatInt(n, 10)
	}
	if err := write(filepath.Join(dir, "memory.max"), orMax(env.Memory)); err != nil {
		return err
	}
	if err := write(filepath.Join(dir, "pids.max"), orMax(env.Pids)); err != nil {
		return err
	}
	if cpu {
		// cpu.max is "<quota> <period>" in microseconds.
		quota := "max"
		if env.CPU > 0 {
			quota = strconv.FormatInt(int64(env.CPU*100000), 10)
		}
		if err := write(filepath.Join(dir, "cpu.max"), quota+" 100000"); err != nil {
			return err
		}
	}
	return nil
}

// Create makes the lease's cgroup, with its limits and its _self leaf.
func (b *Backend) Create(_ context.Context, lease api.LeaseID, parent backend.ACGroupRef, env api.Envelope) (backend.ACGroup, error) {
	if !validLease(lease) {
		return nil, fmt.Errorf("cgroup2: invalid lease id %q", lease)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	parentDir := b.cfg.Base
	if parent.Lease != "" {
		d, ok := b.dirs[parent.Lease]
		if !ok {
			return nil, fmt.Errorf("cgroup2: parent acgroup %s does not exist", parent.Lease)
		}
		parentDir = d
	}
	dir := filepath.Join(parentDir, string(lease))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cgroup2: %w", err)
	}
	fail := func(err error) (backend.ACGroup, error) {
		_ = os.Remove(filepath.Join(dir, selfLeaf))
		_ = os.Remove(dir)
		return nil, fmt.Errorf("cgroup2: create %s: %w", lease, err)
	}
	if err := limits(dir, env, b.caps.CPU == api.LevelHard); err != nil {
		return fail(err)
	}
	// When the limit is hit the whole acgroup dies together, never a
	// single victim inside it.
	if err := write(filepath.Join(dir, "memory.oom.group"), "1"); err != nil {
		return fail(err)
	}
	// With swap the limit would not be a limit on memory. Kernels without
	// the swap controller have no such file.
	if err := write(filepath.Join(dir, "memory.swap.max"), "0"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fail(err)
	}
	if b.caps.CPU == api.LevelHard {
		// Equal weights: leads borrow idle CPU from each other and get
		// equal shares under contention.
		_ = write(filepath.Join(dir, "cpu.weight"), "100")
	}
	if err := os.Mkdir(filepath.Join(dir, selfLeaf), 0o755); err != nil {
		return fail(err)
	}
	if err := b.delegate(dir); err != nil {
		return fail(err)
	}
	b.dirs[lease] = dir
	b.limits[lease] = env.Output
	return &group{lease: lease, dir: dir}, nil
}

// ScratchDir is where a lease's output files and results live.
func (b *Backend) ScratchDir(lease api.LeaseID) string {
	return filepath.Join(b.cfg.StateDir, "scratch", string(lease))
}

type proc struct {
	tty    *os.File
	cmd    *exec.Cmd
	once   sync.Once
	status backend.ExitStatus
}

func (p *proc) Pid() int { return p.cmd.Process.Pid }

// TTY returns the master end of the lead's pseudo-terminal, or nil.
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

// Spawn starts the main process inside the lease's _self leaf. The kernel
// creates the process directly in that cgroup and in a new cgroup
// namespace rooted there, so it is inside the limits and cannot see or
// reach any other cgroup from its first instruction. The process is the
// shim, which drops privileges and then execs the command.
func (b *Backend) Spawn(_ context.Context, g backend.ACGroup, c backend.Cmd) (backend.Proc, error) {
	dir, err := b.dir(g)
	if err != nil {
		return nil, err
	}
	if c.UID == 0 {
		return nil, errors.New("cgroup2: an agent never runs as root")
	}
	u, err := user.LookupId(strconv.Itoa(c.UID))
	if err != nil {
		return nil, fmt.Errorf("cgroup2: uid %d: %w", c.UID, err)
	}
	gid, _ := strconv.Atoi(u.Gid)
	lease := g.Ref().Lease
	scratch := b.ScratchDir(lease)
	if err := os.MkdirAll(scratch, 0o750); err != nil {
		return nil, err
	}
	if err := os.Chown(scratch, c.UID, gid); err != nil {
		return nil, err
	}
	open := func(name string) (*os.File, error) {
		f, err := os.OpenFile(filepath.Join(scratch, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err == nil {
			err = f.Chown(c.UID, gid)
		}
		return f, err
	}
	stdoutFile, err := open("stdout")
	if err != nil {
		return nil, err
	}
	stderrFile, err := open("stderr")
	if err != nil {
		stdoutFile.Close()
		return nil, err
	}
	// The agent writes into pipes; the daemon carries them to the files.
	outR, stdout, err := os.Pipe()
	if err != nil {
		stdoutFile.Close()
		stderrFile.Close()
		return nil, err
	}
	errR, stderr, err := os.Pipe()
	if err != nil {
		stdoutFile.Close()
		stderrFile.Close()
		outR.Close()
		stdout.Close()
		return nil, err
	}
	defer stdout.Close() // the agent holds its own copies of the write ends
	defer stderr.Close()
	leaf, err := os.Open(filepath.Join(dir, selfLeaf))
	if err != nil {
		return nil, err
	}
	defer leaf.Close()

	// The agent keeps its user's supplementary groups: among them is the
	// group that may reach the daemon's socket, without which it could not
	// start child agents.
	groups := strconv.Itoa(gid)
	if ids, err := u.GroupIds(); err == nil && len(ids) > 0 {
		groups = strings.Join(ids, ",")
	}
	args := append([]string{ShimArg, strconv.Itoa(c.UID), strconv.Itoa(gid), groups, strconv.Itoa(agentOOMScoreAdj), "--"}, c.Argv...)
	cmd := exec.Command(b.cfg.Shim, args...)
	cmd.Dir = c.Dir
	cmd.Env = append(append([]string{}, c.Env...),
		"SUBACTL_LEASE_ID="+string(lease),
		"SUBACTL_SOCK="+b.cfg.Socket,
		"SUBACTL_SCRATCH="+scratch,
	)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		UseCgroupFD:  true, // clone3(CLONE_INTO_CGROUP)
		CgroupFD:     int(leaf.Fd()),
		Unshareflags: unix.CLONE_NEWCGROUP,
		Setsid:       true,
	}
	// A lead gets a pseudo-terminal in place of the output files: the
	// daemon keeps the master end and relays it to "subactl run -it".
	var master *os.File
	if c.TTY {
		pair, err := pty.Open()
		if err != nil {
			return nil, fmt.Errorf("cgroup2: %w", err)
		}
		slave, err := os.OpenFile(pair.Slave, os.O_RDWR|unix.O_NOCTTY, 0)
		if err == nil {
			err = os.Chown(pair.Slave, c.UID, gid)
		}
		if err != nil {
			pair.Master.Close()
			return nil, fmt.Errorf("cgroup2: pty slave: %w", err)
		}
		defer slave.Close()
		master = pair.Master
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		cmd.SysProcAttr.Setctty = true // fd 0 becomes the controlling terminal
	}
	if err := cmd.Start(); err != nil {
		if master != nil {
			master.Close()
		}
		for _, f := range []*os.File{outR, errR, stdoutFile, stderrFile} {
			f.Close()
		}
		return nil, fmt.Errorf("cgroup2: spawn in %s: %w", lease, err)
	}
	b.mu.Lock()
	out := newOutput(b.limits[lease])
	b.outputs[lease] = out
	b.mu.Unlock()
	if master != nil {
		// A lead writes to its terminal; the pipes carry nothing.
		for _, f := range []*os.File{outR, errR, stdoutFile, stderrFile} {
			f.Close()
		}
	} else {
		out.carry(outR, stdoutFile)
		out.carry(errR, stderrFile)
	}
	return &proc{cmd: cmd, tty: master}, nil
}

func readInt(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return n
}

// stat reads one key of a flat-keyed file such as memory.stat.
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

// Sample reads the acgroup's usage, nested acgroups included. Memory is the
// working set: memory.current minus inactive file cache.
func (b *Backend) Sample(_ context.Context, g backend.ACGroup) (api.Usage, error) {
	dir, err := b.dir(g)
	if err != nil {
		return api.Usage{}, err
	}
	cur, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return api.Usage{}, fmt.Errorf("cgroup2: sample %s: %w", g.Ref().Lease, err)
	}
	current, _ := strconv.ParseInt(strings.TrimSpace(string(cur)), 10, 64)
	return api.Usage{
		Memory:     max(current-stat(filepath.Join(dir, "memory.stat"), "inactive_file"), 0),
		MemoryPeak: readInt(filepath.Join(dir, "memory.peak")),
		MemBasis:   api.BasisWorkingSet,
		CPUSeconds: float64(stat(filepath.Join(dir, "cpu.stat"), "usage_usec")) / 1e6,
		Pids:       readInt(filepath.Join(dir, "pids.current")),
		OOMEvents:  ownOOMs(dir),
		SampledAt:  time.Now(),
	}, nil
}

// ownOOMs counts the times this acgroup was killed as a group because its
// own limit was hit. The kernel records a group kill on the cgroup whose
// limit caused it, so a nested acgroup that died with its parent reports
// zero. Kernels before 5.19 lack that counter; there "oom", the number of
// times the cgroup's own limit was reached, is the closest fact available.
func ownOOMs(dir string) int64 {
	data, err := os.ReadFile(filepath.Join(dir, "memory.events.local"))
	if err != nil {
		return 0
	}
	vals := map[string]int64{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, " "); ok {
			vals[k], _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if n, ok := vals["oom_group_kill"]; ok {
		return n
	}
	return vals["oom"]
}

// RemoveScratch deletes a lease's scratch directory.
func (b *Backend) RemoveScratch(lease api.LeaseID) error {
	if !validLease(lease) {
		return fmt.Errorf("cgroup2: invalid lease id %q", lease)
	}
	return os.RemoveAll(b.ScratchDir(lease))
}

// OutputBytes is how much the lease has written to stdout and stderr, as
// read from its pipes; what passed the limit is counted but not kept.
func (b *Backend) OutputBytes(_ context.Context, g backend.ACGroup) (int64, error) {
	b.mu.Lock()
	out := b.outputs[g.Ref().Lease]
	b.mu.Unlock()
	if out == nil {
		return 0, nil
	}
	return out.read.Load(), nil
}

// SetLimits applies a changed envelope.
func (b *Backend) SetLimits(_ context.Context, g backend.ACGroup, env api.Envelope) error {
	dir, err := b.dir(g)
	if err != nil {
		return err
	}
	if err := limits(dir, env, b.caps.CPU == api.LevelHard); err != nil {
		return fmt.Errorf("cgroup2: set limits of %s: %w", g.Ref().Lease, err)
	}
	b.mu.Lock()
	b.limits[g.Ref().Lease] = env.Output
	if out := b.outputs[g.Ref().Lease]; out != nil {
		out.limit.Store(env.Output)
	}
	b.mu.Unlock()
	return nil
}

func populated(dir string) bool {
	return stat(filepath.Join(dir, "cgroup.events"), "populated") != 0
}

// KillTree kills every process in the acgroup and below it, atomically,
// and verifies that none is left.
func (b *Backend) KillTree(ctx context.Context, g backend.ACGroup) (backend.KillReport, error) {
	dir, err := b.dir(g)
	if err != nil {
		return backend.KillReport{}, err
	}
	rep := backend.KillReport{Killed: int(readInt(filepath.Join(dir, "pids.current")))}
	if err := write(filepath.Join(dir, "cgroup.kill"), "1"); err != nil {
		return rep, fmt.Errorf("cgroup2: kill %s: %w", g.Ref().Lease, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for populated(dir) {
		if time.Now().After(deadline) || ctx.Err() != nil {
			rep.Remaining = int(readInt(filepath.Join(dir, "pids.current")))
			rep.Killed -= rep.Remaining
			return rep, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return rep, nil
}

// Destroy removes the acgroup. Its nested acgroups must be gone already.
func (b *Backend) Destroy(_ context.Context, g backend.ACGroup) error {
	dir, err := b.dir(g)
	if err != nil {
		return err
	}
	// rmdir of a cgroup fails with EBUSY for a moment after its last
	// process died, until the kernel has released it.
	var rerr error
	for range 200 {
		rerr = os.Remove(filepath.Join(dir, selfLeaf))
		if rerr == nil || errors.Is(rerr, fs.ErrNotExist) {
			rerr = os.Remove(dir)
		}
		if rerr == nil || errors.Is(rerr, fs.ErrNotExist) {
			b.mu.Lock()
			out := b.outputs[g.Ref().Lease]
			delete(b.dirs, g.Ref().Lease)
			delete(b.outputs, g.Ref().Lease)
			delete(b.limits, g.Ref().Lease)
			b.mu.Unlock()
			if out != nil {
				out.close() // every process is gone; the files are complete
			}
			return nil
		}
		if !errors.Is(rerr, syscall.EBUSY) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("cgroup2: destroy %s: %w", g.Ref().Lease, rerr)
}

// leaseOf maps a cgroup path as seen from the root namespace (the third
// field of /proc/<pid>/cgroup) to the lease whose acgroup it is in.
func (b *Backend) leaseOf(cgPath string) (api.LeaseID, bool) {
	rel, err := filepath.Rel(b.cfg.Base, filepath.Join(cgroupRoot, cgPath))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	// A process sits in <lease>/_self; take the innermost lease.
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != selfLeaf {
			return api.LeaseID(parts[i]), true
		}
	}
	return "", false
}

// Identify maps a pid to the acgroup it is a member of. Membership in a
// cgroup cannot be forged by the process, which is why it serves as the
// caller's identity.
func (b *Backend) Identify(_ context.Context, pid int) (backend.ACGroupRef, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return backend.ACGroupRef{}, fmt.Errorf("cgroup2: identify pid %d: %w", pid, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		// The unified hierarchy's line is "0::<path>".
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			if lease, ok := b.leaseOf(path); ok {
				return backend.ACGroupRef{Lease: lease}, nil
			}
		}
	}
	return backend.ACGroupRef{}, backend.ErrNotMember
}

// List reports every acgroup under the base, including those left by a
// previous daemon, and remembers where each one is.
func (b *Backend) List(context.Context) ([]backend.ACGroupRef, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []backend.ACGroupRef
	err := filepath.WalkDir(b.cfg.Base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || path == b.cfg.Base {
			return nil
		}
		if d.Name() == selfLeaf {
			return filepath.SkipDir
		}
		lease := api.LeaseID(d.Name())
		b.dirs[lease] = path
		out = append(out, backend.ACGroupRef{Lease: lease})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cgroup2: list: %w", err)
	}
	return out, nil
}

// ScanEscapes looks for processes that carry SUBACTL_LEASE_ID in their
// environment but are not inside the base cgroup. Reading another
// process's environment needs root, which the daemon has.
func (b *Backend) ScanEscapes(ctx context.Context) ([]backend.Escape, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	const marker = "SUBACTL_LEASE_ID="
	var out []backend.Escape
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if _, err := b.Identify(ctx, pid); !errors.Is(err, backend.ErrNotMember) {
			continue // inside an acgroup, or already gone
		}
		env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil {
			continue
		}
		for _, kv := range strings.Split(string(env), "\x00") {
			if lease, ok := strings.CutPrefix(kv, marker); ok && validLease(api.LeaseID(lease)) {
				out = append(out, backend.Escape{Pid: pid, Lease: api.LeaseID(lease)})
				break
			}
		}
	}
	return out, nil
}

// WatchHolder reports the exit of a process through a pidfd, which is
// exact and free of pid-reuse races once it is open.
func (b *Backend) WatchHolder(ctx context.Context, pid int) (<-chan struct{}, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, fmt.Errorf("cgroup2: watch pid %d: %w", pid, err)
	}
	gone := make(chan struct{})
	go func() {
		defer unix.Close(fd)
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for ctx.Err() == nil {
			// A pidfd becomes readable when its process exits. Poll with a
			// timeout so that a cancelled watch ends.
			n, err := unix.Poll(fds, 500)
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

// ShimMain is the pre-exec shim: subactld re-executed inside the lease's
// cgroup and cgroup namespace, still as root. It fixes the OOM score
// floor, drops to the agent's user, forbids any later gain of privilege
// and execs the command. It returns only on error.
//
//	subactld __exec <uid> <gid> <gid,gid,...> <oom_score_adj> -- <command> [args...]
func ShimMain(args []string) error {
	if len(args) < 6 || args[4] != "--" {
		return errors.New("shim: usage: __exec UID GID GROUPS OOM_SCORE_ADJ -- COMMAND [ARGS]")
	}
	uid, err1 := strconv.Atoi(args[0])
	gid, err2 := strconv.Atoi(args[1])
	if err1 != nil || err2 != nil || uid == 0 {
		return errors.New("shim: a non-root uid and a gid are required")
	}
	var groups []int
	for _, g := range strings.Split(args[2], ",") {
		n, err := strconv.Atoi(g)
		if err != nil || n == 0 {
			continue // never the root group
		}
		groups = append(groups, n)
	}
	// Written while privileged, the value is also the floor.
	if err := write("/proc/self/oom_score_adj", args[3]); err != nil {
		return fmt.Errorf("shim: %w", err)
	}
	if err := syscall.Setgroups(groups); err != nil {
		return fmt.Errorf("shim: setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("shim: setgid: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("shim: setuid: %w", err)
	}
	// No process of the lease may gain privileges again: setuid programs
	// such as sudo run without their set-user-ID bit. An agent that became
	// root could leave its cgroup namespace and undo every limit. This is
	// inherited by every descendant and cannot be turned off.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("shim: no_new_privs: %w", err)
	}
	argv := args[5:]
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("shim: %w", err)
	}
	return syscall.Exec(path, argv, os.Environ())
}

// TopProcs lists the processes of the acgroup and of those nested in it,
// largest resident set first, at most n.
func (b *Backend) TopProcs(_ context.Context, g backend.ACGroup, n int) ([]api.Proc, error) {
	dir, err := b.dir(g)
	if err != nil {
		return nil, err
	}
	var out []api.Proc
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "cgroup.procs" {
			return nil
		}
		data, _ := os.ReadFile(path)
		for _, f := range strings.Fields(string(data)) {
			pid, _ := strconv.Atoi(f)
			comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
			if err != nil {
				continue // gone meanwhile
			}
			status, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
			var rss int64
			for _, line := range strings.Split(string(status), "\n") {
				if v, ok := strings.CutPrefix(line, "VmRSS:"); ok {
					kb, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
					rss = kb << 10
				}
			}
			out = append(out, api.Proc{Pid: pid, Name: strings.TrimSpace(string(comm)), Memory: rss})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Memory > out[j].Memory })
	return out[:min(n, len(out))], nil
}
