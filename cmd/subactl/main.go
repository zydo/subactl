// Command subactl is the CLI of subactld. It is the only way to create an
// agent: a person starts a lead with "subactl run", and an agent spawns
// child agents with "subactl spawn" from its bash tool.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/client"
)

const usage = `usage: subactl [--socket PATH] <command> [flags] [-- command...]

  host                         effective capacity, allocation and capability matrix
  run [flags] -- CMD...        start a lead agent and wait for it
  spawn [flags] -- CMD...      start a child agent under the current lease
  available [LEASE]            remaining budget of a subtree (default: own lease)
  ls [--lead LEASE]            lease trees visible to the caller
  get LEASE                    one lease, with usage and termination record
  resize LEASE [flags]         change the envelope of a lease in your subtree
  terminate LEASE [flags]      graceful: wait, SIGTERM, then kill the whole tree
  kill LEASE                   kill the whole tree at once (499)
  wait-children [LEASE]        wait for the subtree to end (default: own lease)
  suggest KIND [--lease LEASE] suggested memory for a kind, in bytes: p95 of past peaks x 1.2
  trace LEASE                  timeline of a lease: usage series, events, termination
  audit --lease LEASE          the same timeline, for a lease that ended long ago
  events                       follow the events you may see, one JSON object per line
  notify LEASE --type T        put a control message in a descendant's inbox
  inbox                        follow your own lease's control messages
  beacon --state S [--tool T]  report progress: working, awaiting_input or done
  done [--result PATH]         report that the task is finished; PATH is in your scratch directory
  resize-request --memory S    ask your parent for a larger envelope
  metrics                      Prometheus metrics
  top [--once]                 live view of the lead trees you may see; q quits
  completion bash|zsh|fish     print a completion script for that shell

Sizes take a binary suffix (512M, 8G). LEASE may be "self" inside a lease.
Commands that change something accept --dry-run where the daemon supports it.
A command that waits for a lease exits 0 if it ended with a 2xx code, else 3.
`

// pending names commands that land in later milestones.
var pending = map[string]string{}

// stdin is where "run -it" reads what the user types; tests replace it.
var stdin io.Reader = os.Stdin

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	global := flag.NewFlagSet("subactl", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() { fmt.Fprint(stderr, usage) }
	socket := global.String("socket", client.SocketFromEnv(), "Unix socket of subactld")
	if err := global.Parse(args); err != nil {
		return 2
	}
	args = global.Args()
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, args := args[0], args[1:]
	if ms, ok := pending[cmd]; ok {
		fmt.Fprintf(stderr, "subactl %s lands in milestone %s\n", cmd, ms)
		return 2
	}
	c := &cli{api: client.New(*socket), out: stdout, err: stderr}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	handlers := map[string]func(context.Context, []string) (int, error){
		"host": c.host, "run": c.run, "spawn": c.spawn, "available": c.available,
		"ls": c.ls, "get": c.get, "resize": c.resize, "terminate": c.terminate,
		"kill": c.kill, "wait-children": c.waitChildren,
		"notify": c.notify, "inbox": c.inbox, "beacon": c.beacon, "done": c.done, "resize-request": c.resizeRequest,
		"top":        c.top,
		"completion": c.completion,
		"suggest":    c.suggest, "trace": c.trace, "audit": c.audit, "events": c.events, "metrics": c.metrics,
	}
	h, ok := handlers[cmd]
	if !ok {
		fmt.Fprintf(stderr, "subactl: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	code, err := h(ctx, args)
	if err == nil {
		return code
	}
	if errors.Is(err, flag.ErrHelp) {
		return 2
	}
	// Errors are structured so that an LLM caller can correct itself.
	var ae *api.Error
	if !errors.As(err, &ae) {
		ae = &api.Error{Code: "cli", Message: err.Error()}
	}
	enc := json.NewEncoder(stderr)
	enc.SetIndent("", "  ")
	_ = enc.Encode(ae)
	if ae.Code == "cli" {
		return 2
	}
	return 1
}

type cli struct {
	api *client.Client
	out io.Writer
	err io.Writer
}

func (c *cli) print(v any) {
	enc := json.NewEncoder(c.out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (c *cli) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("subactl "+name, flag.ContinueOnError)
	fs.SetOutput(c.err)
	return fs
}

// leaseArg takes a leading lease id off args, so that flags may follow it.
// Without one it falls back to def.
func leaseArg(args []string, def api.LeaseID) (api.LeaseID, []string, error) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return api.LeaseID(args[0]), args[1:], nil
	}
	if def == "" {
		return "", nil, errors.New("a lease id is required, e.g. ls_7f3a9c21")
	}
	return def, args, nil
}

// sizeFlag is a flag holding a byte size such as 8G.
type sizeFlag struct {
	n   int64
	set bool
}

func (s *sizeFlag) String() string { return api.FormatBytes(s.n) }
func (s *sizeFlag) Set(v string) error {
	n, err := api.ParseBytes(v)
	s.n, s.set = n, err == nil
	return err
}

func (s *sizeFlag) ptr() *int64 {
	if !s.set {
		return nil
	}
	return &s.n
}

// envelopeFlags registers the envelope dimensions on fs.
type envelopeFlags struct {
	memory, memorySelf, output sizeFlag
	cpu                        float64
	pids                       int64
	wall                       time.Duration
	fs                         *flag.FlagSet
}

func addEnvelope(fs *flag.FlagSet) *envelopeFlags {
	e := &envelopeFlags{fs: fs}
	fs.Var(&e.memory, "memory", "memory hard limit, e.g. 8G")
	fs.Var(&e.memorySelf, "memory-self", "memory kept for the node's own processes")
	fs.Var(&e.output, "output", "stdout and stderr limit, e.g. 100M")
	fs.Float64Var(&e.cpu, "cpu", 0, "CPU cores (0 is unlimited)")
	fs.Int64Var(&e.pids, "pids", 0, "process limit (0 is unlimited)")
	fs.DurationVar(&e.wall, "wall", 0, "wall-time limit, e.g. 30m (0 is unlimited)")
	return e
}

func (e *envelopeFlags) envelope() api.Envelope {
	return api.Envelope{
		Memory: e.memory.n, MemorySelf: e.memorySelf.n, Output: e.output.n,
		CPU: e.cpu, Pids: e.pids, Wall: int64(e.wall / time.Second),
	}
}

// patch holds only the dimensions that were given on the command line.
func (e *envelopeFlags) patch() api.EnvelopePatch {
	p := api.EnvelopePatch{Memory: e.memory.ptr(), MemorySelf: e.memorySelf.ptr(), Output: e.output.ptr()}
	e.fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "cpu":
			p.CPU = &e.cpu
		case "pids":
			p.Pids = &e.pids
		case "wall":
			w := int64(e.wall / time.Second)
			p.Wall = &w
		}
	})
	return p
}

func (c *cli) host(ctx context.Context, args []string) (int, error) {
	if err := c.flags("host").Parse(args); err != nil {
		return 2, err
	}
	h, err := c.api.Host(ctx)
	if err != nil {
		return 1, err
	}
	c.print(h)
	return 0, nil
}

// run and spawn share everything except who the parent is: run starts a
// lead and always waits for it; spawn starts a child.
func (c *cli) run(ctx context.Context, args []string) (int, error) {
	return c.start(ctx, "run", args)
}

func (c *cli) spawn(ctx context.Context, args []string) (int, error) {
	return c.start(ctx, "spawn", args)
}

func (c *cli) start(ctx context.Context, name string, args []string) (int, error) {
	fs := c.flags(name)
	env := addEnvelope(fs)
	kind := fs.String("kind", "", "kind of agent, e.g. worker; groups history for suggestions")
	requireMem := fs.String("require-memory", "", "weakest acceptable memory enforcement: hard, soft or monitor_only")
	killOrder := fs.Int("kill-order", 0, "safety-net order hint; lower is terminated first")
	var growTo sizeFlag
	fs.Var(&growTo, "grow-to", "on NEAR_LIMIT, grow the memory limit up to this size if the parent has room")
	dryRun := fs.Bool("dry-run", false, "check admission without reserving or starting anything")
	uid := fs.Int("uid", -1, "uid the lead runs as (required when the operator is root)")
	parent := fs.String("parent", "", "parent lease (default: the caller's own lease)")
	wait := fs.Bool("wait", name == "run", "block until the lease ends and print its termination record")
	interactive := fs.Bool("it", false, "give the lead a terminal and relay it to this one, like docker run -it")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if *interactive && name != "run" {
		return 2, errors.New("-it attaches a terminal to a lead; child agents are headless")
	}
	argv := fs.Args()
	if len(argv) == 0 && !*dryRun {
		return 2, fmt.Errorf("no command given; usage: subactl %s [flags] -- CMD ARGS", name)
	}
	req := api.AcquireRequest{Kind: *kind, Envelope: env.envelope(), DryRun: *dryRun, TTY: *interactive}
	if name == "spawn" {
		req.Parent = api.LeaseID(*parent)
	} else if *parent != "" {
		return 2, errors.New("run starts a lead, which has no parent; use spawn for a child")
	}
	if *requireMem != "" {
		req.Require = &api.Require{Memory: api.Level(*requireMem)}
	}
	if growTo.set {
		req.OnNearLimit = &api.OnNearLimit{GrowTo: growTo.n}
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "kill-order":
			req.KillOrder = killOrder
		case "uid":
			req.UID = uid
		}
	})

	ls, err := c.api.Acquire(ctx, req)
	if err != nil {
		return 1, err
	}
	if *dryRun {
		c.print(ls)
		return 0, nil
	}
	// From here on the lease exists. If this process is interrupted while it
	// waits, the lease must not be left behind. For a lead this process is
	// the holder: where the backend detects a holder's death exactly, going
	// away is all it takes, and the tree is reaped as 502 PARENT_DIED, the
	// same as when the terminal is closed. Everywhere else the lease is
	// killed explicitly.
	cleanup := func() {
		if name == "run" {
			if h, err := c.api.Host(context.Background()); err == nil && h.Capabilities.ParentDeath == api.LevelHard && h.Backend != "fake" {
				return
			}
		}
		_, _ = c.api.Kill(context.Background(), ls.ID)
	}
	cwd, _ := os.Getwd()
	if _, err := c.api.Spawn(ctx, ls.ID, api.SpawnRequest{Argv: argv, Env: os.Environ(), Dir: cwd}); err != nil {
		cleanup()
		return 1, err
	}
	if *interactive {
		return c.interactive(ctx, ls.ID, cleanup)
	}
	if !*wait {
		c.print(ls)
		return 0, nil
	}
	// A lead's output is what the person who started it wants to see. It
	// is written to the lease's scratch directory, which goes away with the
	// lease, so it is shown here while it is written. The termination
	// record follows on stderr, where it does not mix with the output.
	stopFollowing := func() {}
	if name == "run" {
		if full, err := c.api.Lease(ctx, ls.ID); err == nil && full.Scratch != "" {
			stop := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				follow(full.Scratch, c.out, c.err, stop)
			}()
			var once sync.Once
			stopFollowing = func() { once.Do(func() { close(stop); <-done }) }
			defer stopFollowing()
		}
	}
	res, err := c.api.WaitChildren(ctx, ls.ID, api.WaitChildrenRequest{Self: true})
	// The rest of the output first, then anything about the end of it.
	stopFollowing()
	if ctx.Err() != nil {
		cleanup()
		return 130, errors.New("interrupted; the lease was killed")
	}
	if err != nil {
		return 1, err
	}
	if name == "run" {
		return c.reportEnd(res.Results[0])
	}
	return c.printTermination(res.Results[0])
}

// reportEnd writes a lead's termination record to stderr and exits by it.
func (c *cli) reportEnd(t api.Termination) (int, error) {
	enc := json.NewEncoder(c.err)
	enc.SetIndent("", "  ")
	_ = enc.Encode(t)
	if t.Code >= 200 && t.Code < 300 {
		return 0, nil
	}
	return 3, nil
}

// follow copies what is appended to the stdout and stderr files of a
// scratch directory until stop is closed, then once more to the end. A
// file that cannot be read, for want of permission or because it is
// already gone, is skipped: the output is a convenience, the lease's fate
// does not depend on it.
func follow(scratch string, out, errw io.Writer, stop <-chan struct{}) {
	type src struct {
		name string
		w    io.Writer
		f    *os.File
	}
	srcs := []*src{{name: "stdout", w: out}, {name: "stderr", w: errw}}
	buf := make([]byte, 32<<10)
	drain := func() {
		for _, s := range srcs {
			if s.f == nil {
				f, err := os.Open(filepath.Join(scratch, s.name))
				if err != nil {
					continue
				}
				s.f = f
			}
			for {
				n, err := s.f.Read(buf)
				if n > 0 {
					_, _ = s.w.Write(buf[:n])
				}
				if err != nil || n == 0 {
					break
				}
			}
		}
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		drain()
		select {
		case <-stop:
			drain()
			for _, s := range srcs {
				if s.f != nil {
					s.f.Close()
				}
			}
			return
		case <-tick.C:
		}
	}
}

func (c *cli) printTermination(t api.Termination) (int, error) {
	c.print(t)
	if t.Code >= 200 && t.Code < 300 {
		return 0, nil
	}
	return 3, nil
}

func (c *cli) available(ctx context.Context, args []string) (int, error) {
	id, rest, _ := leaseArg(args, api.Self)
	if err := c.flags("available").Parse(rest); err != nil {
		return 2, err
	}
	a, err := c.api.Available(ctx, id)
	if err != nil {
		return 1, err
	}
	c.print(a)
	return 0, nil
}

func (c *cli) ls(ctx context.Context, args []string) (int, error) {
	fs := c.flags("ls")
	lead := fs.String("lead", "", "show only this lead's tree")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	leases, err := c.api.Leases(ctx, api.LeaseID(*lead))
	if err != nil {
		return 1, err
	}
	c.print(leases)
	return 0, nil
}

func (c *cli) get(ctx context.Context, args []string) (int, error) {
	id, rest, err := leaseArg(args, "")
	if err != nil {
		return 2, err
	}
	if err := c.flags("get").Parse(rest); err != nil {
		return 2, err
	}
	ls, err := c.api.Lease(ctx, id)
	if err != nil {
		return 1, err
	}
	c.print(ls)
	return 0, nil
}

func (c *cli) resize(ctx context.Context, args []string) (int, error) {
	id, rest, err := leaseArg(args, "")
	if err != nil {
		return 2, err
	}
	fs := c.flags("resize")
	env := addEnvelope(fs)
	dryRun := fs.Bool("dry-run", false, "check the change without applying it")
	if err := fs.Parse(rest); err != nil {
		return 2, err
	}
	p := env.patch()
	p.DryRun = *dryRun
	ls, err := c.api.Resize(ctx, id, p)
	if err != nil {
		return 1, err
	}
	c.print(ls)
	return 0, nil
}

func (c *cli) terminate(ctx context.Context, args []string) (int, error) {
	id, rest, err := leaseArg(args, "")
	if err != nil {
		return 2, err
	}
	fs := c.flags("terminate")
	grace := fs.Duration("grace", 0, "how long to wait before SIGTERM (default: the daemon's 30s)")
	code := fs.Int("code", api.CodeKilled, "termination code to record: 204 (released) or 499 (killed)")
	reason := fs.String("reason", "", "free text shown to the terminated agent")
	recursive := fs.Bool("recursive", false, "also notify the descendants")
	dryRun := fs.Bool("dry-run", false, "check the request without terminating anything")
	if err := fs.Parse(rest); err != nil {
		return 2, err
	}
	req := api.TerminateRequest{Reason: *reason, Code: *code, Recursive: *recursive, DryRun: *dryRun}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "grace" {
			s := grace.Seconds()
			req.GraceSeconds = &s
		}
	})
	t, err := c.api.Terminate(ctx, id, req)
	if err != nil {
		return 1, err
	}
	if *dryRun {
		return c.dryRunOK()
	}
	c.print(t)
	return 0, nil
}

// dryRunOK reports a dry run that passed.
func (c *cli) dryRunOK() (int, error) {
	fmt.Fprintln(c.out, "ok: the request is valid; nothing was changed")
	return 0, nil
}

func (c *cli) kill(ctx context.Context, args []string) (int, error) {
	id, rest, err := leaseArg(args, "")
	if err != nil {
		return 2, err
	}
	fs := c.flags("kill")
	dryRun := fs.Bool("dry-run", false, "check the request without killing anything")
	if err := fs.Parse(rest); err != nil {
		return 2, err
	}
	t, err := c.api.Kill(ctx, id, api.KillRequest{DryRun: *dryRun})
	if err != nil {
		return 1, err
	}
	if *dryRun {
		return c.dryRunOK()
	}
	c.print(t)
	return 0, nil
}

func (c *cli) waitChildren(ctx context.Context, args []string) (int, error) {
	id, rest, _ := leaseArg(args, api.Self)
	fs := c.flags("wait-children")
	self := fs.Bool("self", false, "also wait for the lease itself")
	timeout := fs.Duration("timeout", 0, "give up after this long (0 waits forever)")
	if err := fs.Parse(rest); err != nil {
		return 2, err
	}
	res, err := c.api.WaitChildren(ctx, id, api.WaitChildrenRequest{Self: *self, TimeoutSeconds: timeout.Seconds()})
	if err != nil {
		return 1, err
	}
	c.print(res)
	if !res.Done {
		return 3, nil
	}
	for _, t := range res.Results {
		if t.Code < 200 || t.Code >= 300 {
			return 3, nil
		}
	}
	return 0, nil
}

// suggest prints one number, so that it can be used in place:
//
//	subactl spawn --memory "$(subactl suggest worker)" -- ...
//
// Whether the number comes from history or is the default goes to stderr.
func (c *cli) suggest(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return 2, errors.New("a kind is required, e.g. subactl suggest worker")
	}
	kind := args[0]
	fs := c.flags("suggest")
	lease := fs.String("lease", string(api.Self), "the lease whose view to use; operators must name one of their leads")
	if err := fs.Parse(args[1:]); err != nil {
		return 2, err
	}
	a, err := c.api.Available(ctx, api.LeaseID(*lease), kind)
	if err != nil {
		return 1, err
	}
	s := a.Suggest[kind]
	fmt.Fprintln(c.out, s.Memory)
	if s.Default {
		fmt.Fprintf(c.err, "%s: default of %s; only %d samples of this kind so far\n", kind, api.FormatBytes(s.Memory), s.Samples)
	} else {
		fmt.Fprintf(c.err, "%s: %s from %d samples (%s)\n", kind, api.FormatBytes(s.Memory), s.Samples, s.MemBasis)
	}
	return 0, nil
}

func (c *cli) trace(ctx context.Context, args []string) (int, error) {
	id, rest, err := leaseArg(args, "")
	if err != nil {
		return 2, err
	}
	if err := c.flags("trace").Parse(rest); err != nil {
		return 2, err
	}
	tr, err := c.api.Trace(ctx, id)
	if err != nil {
		return 1, err
	}
	c.print(tr)
	return 0, nil
}

func (c *cli) audit(ctx context.Context, args []string) (int, error) {
	fs := c.flags("audit")
	lease := fs.String("lease", "", "the lease whose timeline to rebuild")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if *lease == "" {
		return 2, errors.New("--lease is required, e.g. subactl audit --lease ls_7f3a9c21")
	}
	return c.trace(ctx, []string{*lease})
}

func (c *cli) events(ctx context.Context, args []string) (int, error) {
	if err := c.flags("events").Parse(args); err != nil {
		return 2, err
	}
	enc := json.NewEncoder(c.out)
	err := c.api.Events(ctx, func(e api.Event) error { return enc.Encode(e) })
	if err != nil && ctx.Err() == nil {
		return 1, err
	}
	return 0, nil
}

func (c *cli) metrics(ctx context.Context, args []string) (int, error) {
	if err := c.flags("metrics").Parse(args); err != nil {
		return 2, err
	}
	text, err := c.api.Metrics(ctx)
	if err != nil {
		return 1, err
	}
	fmt.Fprint(c.out, text)
	return 0, nil
}

func (c *cli) notify(ctx context.Context, args []string) (int, error) {
	id, rest, err := leaseArg(args, "")
	if err != nil {
		return 2, err
	}
	fs := c.flags("notify")
	typ := fs.String("type", "", "TERMINATE_REQUESTED, NEAR_LIMIT, RESIZED or CAPACITY_SHRUNK")
	reason := fs.String("reason", "", "free text shown to the agent")
	dryRun := fs.Bool("dry-run", false, "check the request without sending anything")
	if err := fs.Parse(rest); err != nil {
		return 2, err
	}
	if err := c.api.Notify(ctx, id, api.NotifyRequest{Type: *typ, Reason: *reason, DryRun: *dryRun}); err != nil {
		return 1, err
	}
	if *dryRun {
		return c.dryRunOK()
	}
	return 0, nil
}

func (c *cli) inbox(ctx context.Context, args []string) (int, error) {
	if err := c.flags("inbox").Parse(args); err != nil {
		return 2, err
	}
	enc := json.NewEncoder(c.out)
	err := c.api.Inbox(ctx, api.Self, func(m api.InboxMessage) error { return enc.Encode(m) })
	if err != nil && ctx.Err() == nil {
		return 1, err
	}
	return 0, nil
}

func (c *cli) beacon(ctx context.Context, args []string) (int, error) {
	fs := c.flags("beacon")
	state := fs.String("state", "working", "working, awaiting_input or done")
	tool := fs.String("tool", "", "name of the tool about to run; never its arguments")
	seq := fs.Int64("seq", 0, "tool-call sequence number")
	turns := fs.Int64("turns", 0, "turns so far")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if err := c.api.Progress(ctx, api.Self, api.ProgressRequest{State: *state, Tool: *tool, Seq: *seq, Turns: *turns}); err != nil {
		return 1, err
	}
	return 0, nil
}

func (c *cli) done(ctx context.Context, args []string) (int, error) {
	fs := c.flags("done")
	result := fs.String("result", "", "path of the result file, inside your scratch directory ($SUBACTL_SCRATCH)")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if err := c.api.Progress(ctx, api.Self, api.ProgressRequest{State: "done", Result: *result}); err != nil {
		return 1, err
	}
	return 0, nil
}

func (c *cli) resizeRequest(ctx context.Context, args []string) (int, error) {
	fs := c.flags("resize-request")
	var memory sizeFlag
	fs.Var(&memory, "memory", "the memory limit you are asking for, e.g. 6G")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if !memory.set {
		return 2, errors.New("--memory is required, e.g. subactl resize-request --memory 6G")
	}
	if err := c.api.RequestResize(ctx, api.Self, api.ResizeAsk{Memory: memory.n}); err != nil {
		return 1, err
	}
	return 0, nil
}

// interactive relays the lead's terminal to the user's until the lead
// ends. In a real terminal the local side goes to raw mode, so that every
// key, Ctrl-C included, reaches the agent, and window size changes follow.
func (c *cli) interactive(ctx context.Context, id api.LeaseID, cleanup func()) (int, error) {
	t, err := c.api.Attach(ctx, id)
	if err != nil {
		cleanup()
		return 1, err
	}
	defer t.Close()
	in := stdin
	restore := func() {}
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fd := int(f.Fd())
		if old, err := term.MakeRaw(fd); err == nil {
			restore = func() { _ = term.Restore(fd, old) }
		}
		resize := func() {
			if cols, rows, err := term.GetSize(fd); err == nil {
				_ = t.Resize(uint16(rows), uint16(cols))
			}
		}
		resize()
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				resize()
			}
		}()
	}
	defer restore()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			if n > 0 && t.Type(buf[:n]) != nil {
				return
			}
			if err != nil {
				return // end of input: the lead runs on until it ends by itself
			}
		}
	}()
	out := make(chan struct{})
	go func() {
		_, _ = io.Copy(c.out, t)
		close(out)
	}()
	select {
	case <-out:
	case <-ctx.Done():
		restore()
		cleanup()
		return 130, errors.New("interrupted; the lease was killed")
	}
	restore()
	res, err := c.api.WaitChildren(context.Background(), id, api.WaitChildrenRequest{Self: true, TimeoutSeconds: 30})
	if err != nil || len(res.Results) == 0 {
		return 1, err
	}
	term := res.Results[0]
	fmt.Fprintf(c.err, "\r\nlease %s ended: %d %s\n", term.Lease, term.Code, term.Name)
	if term.Code >= 200 && term.Code < 300 {
		return 0, nil
	}
	return 3, nil
}

// commands is what the completion scripts offer.
var commands = []string{
	"host", "run", "spawn", "available", "ls", "get", "resize", "terminate", "kill", "wait-children",
	"suggest", "trace", "audit", "events", "notify", "inbox", "beacon", "done", "resize-request",
	"metrics", "top", "completion",
}

// completion prints a completion script. subactl itself is a plain
// executable and needs no shell function, alias or sourcing to work; the
// scripts only add tab completion of its commands.
func (c *cli) completion(_ context.Context, args []string) (int, error) {
	if len(args) != 1 {
		return 2, errors.New("name a shell: subactl completion bash|zsh|fish")
	}
	list := strings.Join(commands, " ")
	switch args[0] {
	case "bash":
		fmt.Fprintf(c.out, `_subactl() {
  if [ "$COMP_CWORD" -eq 1 ]; then
    COMPREPLY=($(compgen -W "%s" -- "${COMP_WORDS[COMP_CWORD]}"))
  else
    COMPREPLY=($(compgen -f -- "${COMP_WORDS[COMP_CWORD]}"))
  fi
}
complete -F _subactl subactl
`, list)
	case "zsh":
		fmt.Fprintf(c.out, `#compdef subactl
_subactl() {
  if (( CURRENT == 2 )); then
    compadd %s
  else
    _files
  fi
}
compdef _subactl subactl
`, list)
	case "fish":
		for _, name := range commands {
			fmt.Fprintf(c.out, "complete -c subactl -n __fish_use_subcommand -a %s\n", name)
		}
	default:
		return 2, fmt.Errorf("unknown shell %q: want bash, zsh or fish", args[0])
	}
	return 0, nil
}
