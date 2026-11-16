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
	"strings"
	"syscall"
	"time"

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

Sizes take a binary suffix (512M, 8G). LEASE may be "self" inside a lease.
Commands that change something accept --dry-run where the daemon supports it.
A command that waits for a lease exits 0 if it ended with a 2xx code, else 3.
`

// pending names commands that land in later milestones.
var pending = map[string]string{
	"suggest": "M3", "trace": "M3", "audit": "M3", "top": "M4", "done": "M4",
}

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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	handlers := map[string]func(context.Context, []string) (int, error){
		"host": c.host, "run": c.run, "spawn": c.spawn, "available": c.available,
		"ls": c.ls, "get": c.get, "resize": c.resize, "terminate": c.terminate,
		"kill": c.kill, "wait-children": c.waitChildren,
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
	interactive := fs.Bool("it", false, "attach a pty to the lead")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if *interactive {
		return 2, errors.New("-it (the lead's pty) lands in milestone M4")
	}
	argv := fs.Args()
	if len(argv) == 0 && !*dryRun {
		return 2, fmt.Errorf("no command given; usage: subactl %s [flags] -- CMD ARGS", name)
	}
	req := api.AcquireRequest{Kind: *kind, Envelope: env.envelope(), DryRun: *dryRun}
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
	// waits, the lease is killed rather than left behind.
	cleanup := func() { _, _ = c.api.Kill(context.Background(), ls.ID) }
	cwd, _ := os.Getwd()
	if _, err := c.api.Spawn(ctx, ls.ID, api.SpawnRequest{Argv: argv, Env: os.Environ(), Dir: cwd}); err != nil {
		cleanup()
		return 1, err
	}
	if !*wait {
		c.print(ls)
		return 0, nil
	}
	res, err := c.api.WaitChildren(ctx, ls.ID, api.WaitChildrenRequest{Self: true})
	if ctx.Err() != nil {
		cleanup()
		return 130, errors.New("interrupted; the lease was killed")
	}
	if err != nil {
		return 1, err
	}
	return c.printTermination(res.Results[0])
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
	if err := fs.Parse(rest); err != nil {
		return 2, err
	}
	req := api.TerminateRequest{Reason: *reason, Code: *code, Recursive: *recursive}
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
	c.print(t)
	return 0, nil
}

func (c *cli) kill(ctx context.Context, args []string) (int, error) {
	id, rest, err := leaseArg(args, "")
	if err != nil {
		return 2, err
	}
	if err := c.flags("kill").Parse(rest); err != nil {
		return 2, err
	}
	t, err := c.api.Kill(ctx, id)
	if err != nil {
		return 1, err
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
