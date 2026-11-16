// Package scenarios holds the scripted scenarios: the parameters of a run
// (scenario.yaml), the scripts the scripted brain plays for each role, and
// the shell files that make up a task.
package scenarios

import (
	"embed"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/examples/mini-agent/llm"
	"github.com/zydo/subactl/yamlite"
)

//go:embed files
var files embed.FS

// Scenario is one scenario.yaml. Every run with the same file behaves the
// same: there is no randomness beyond Seed.
type Scenario struct {
	Seed int64
	// Task names the task definition under files/: "toy" or "radix".
	Task string
	// Fanout is how many workers the lead runs at once (G1: up to 32).
	Fanout int
	// Depth is how deep recursion goes: a worker whose tests fail spawns a
	// fixer, down to this depth (G1: 3). The lead is depth 0.
	Depth int
	// MaxPackages caps how many packages are dispatched; zero means all.
	MaxPackages int
	// LeakMB is retained by every agent on every step (G3). Zero disables.
	LeakMB int
	// FailRate is the share of (package, depth) pairs whose tests fail.
	FailRate float64

	Duration        time.Duration // the lead ends here
	FanoutAt        time.Duration // G1 fan-out begins
	QuotaAt         time.Duration // G4: the usage limit is hit
	QuotaFor        time.Duration // how long calls keep failing; zero disables G4
	OutputRunawayAt time.Duration // G5 begins

	// External interference, run by the bench and not by any agent: a
	// process outside the agent tree takes InterferenceShare of the
	// capacity within InterferenceRamp and holds it for InterferenceFor.
	InterferenceAt    time.Duration
	InterferenceRamp  time.Duration
	InterferenceFor   time.Duration // zero disables it
	InterferenceShare float64

	LeaveServers      bool // G2: dev servers and browsers are not stopped
	OutputRunaway     bool // G5: one worker writes output without end
	UnboundedParallel bool // G6: tsc and tests at 4x the core count, unbounded file walks
	// UnboundedShare is the share of packages whose worker behaves that
	// way (default 1, all of them). Which ones is fixed by the seed.
	UnboundedShare float64

	// Envelopes gives the memory limit of each kind of agent when it runs
	// under subactl: "lead", "upgrade", "fix" and "runaway". A kind without
	// an entry gets what subactl suggests, or its default.
	Envelopes map[string]int64
	// Adaptive makes workers start from subactl's suggestion for their
	// kind instead of Envelopes, and declare a grow rule (group C).
	Adaptive bool
	// GrowFactor is how far a worker's limit may grow on NEAR_LIMIT, as a
	// multiple of its initial limit. Zero declares no rule.
	GrowFactor float64
	// OutputLimit caps each worker's stdout and stderr under subactl.
	OutputLimit int64

	// Env is extra environment for the task's shell files.
	Env map[string]string
}

// Blank is the scenario of a run that follows no script: a real brain
// decides what to do, and there is no timetable and no task definition.
func Blank() *Scenario {
	return &Scenario{Fanout: 4, Depth: 3, Duration: 24 * time.Hour, UnboundedShare: 1, Env: map[string]string{}, Envelopes: map[string]int64{}}
}

// Load reads a scenario file.
func Load(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sc, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return sc, nil
}

// Parse decodes a scenario. Unknown keys are errors, so a typo cannot
// silently switch a greedy behaviour off.
func Parse(data []byte) (*Scenario, error) {
	doc, err := yamlite.Parse(data)
	if err != nil {
		return nil, err
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("scenario must be a map")
	}
	sc := &Scenario{Task: "toy", Fanout: 4, Depth: 1, Env: map[string]string{}, Envelopes: map[string]int64{}, UnboundedShare: 1}
	d := decoder{m: m}
	d.int64("seed", &sc.Seed)
	d.str("task", &sc.Task)
	d.int("fanout", &sc.Fanout)
	d.int("depth", &sc.Depth)
	d.int("max_packages", &sc.MaxPackages)
	d.int("leak_mb", &sc.LeakMB)
	d.float("fail_rate", &sc.FailRate)
	d.dur("duration", &sc.Duration)
	d.dur("fanout_at", &sc.FanoutAt)
	d.dur("quota_at", &sc.QuotaAt)
	d.dur("quota_for", &sc.QuotaFor)
	d.dur("output_runaway_at", &sc.OutputRunawayAt)
	d.dur("interference_at", &sc.InterferenceAt)
	d.dur("interference_ramp", &sc.InterferenceRamp)
	d.dur("interference_for", &sc.InterferenceFor)
	d.float("interference_share", &sc.InterferenceShare)
	if g, ok := d.take("greedy").(map[string]any); ok {
		gd := decoder{m: g, prefix: "greedy."}
		gd.bool("leave_servers", &sc.LeaveServers)
		gd.bool("output_runaway", &sc.OutputRunaway)
		gd.bool("unbounded_parallel", &sc.UnboundedParallel)
		gd.float("unbounded_share", &sc.UnboundedShare)
		d.errs = append(d.errs, gd.finish()...)
	}
	d.bool("adaptive", &sc.Adaptive)
	d.float("grow_factor", &sc.GrowFactor)
	d.size("output_limit", &sc.OutputLimit)
	if e, ok := d.take("envelopes").(map[string]any); ok {
		ed := decoder{m: e, prefix: "envelopes."}
		for _, kind := range []string{"lead", "upgrade", "fix", "runaway"} {
			var n int64
			ed.size(kind, &n)
			if n > 0 {
				sc.Envelopes[kind] = n
			}
		}
		d.errs = append(d.errs, ed.finish()...)
	}
	if e, ok := d.take("env").(map[string]any); ok {
		for k, v := range e {
			sc.Env[k] = fmt.Sprint(v)
		}
	}
	if errs := d.finish(); len(errs) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	switch {
	case sc.Fanout < 1 || sc.Depth < 1:
		return nil, fmt.Errorf("fanout and depth must be at least 1")
	case sc.Duration <= 0:
		return nil, fmt.Errorf("duration is required, e.g. 15m")
	case sc.FailRate < 0 || sc.FailRate > 1 || sc.UnboundedShare < 0 || sc.UnboundedShare > 1:
		return nil, fmt.Errorf("fail_rate and greedy.unbounded_share must be between 0 and 1")
	case sc.InterferenceShare < 0 || sc.InterferenceShare >= 1:
		return nil, fmt.Errorf("interference_share must be at least 0 and below 1")
	}
	if _, err := fs.Stat(files, "files/"+sc.Task); err != nil {
		return nil, fmt.Errorf("unknown task %q", sc.Task)
	}
	return sc, nil
}

type decoder struct {
	m      map[string]any
	prefix string
	errs   []string
}

func (d *decoder) take(key string) any {
	v := d.m[key]
	delete(d.m, key)
	return v
}

func (d *decoder) bad(key, want string, v any) {
	d.errs = append(d.errs, fmt.Sprintf("%s%s: want %s, got %v", d.prefix, key, want, v))
}

func (d *decoder) int64(key string, dst *int64) {
	switch v := d.take(key).(type) {
	case nil:
	case int64:
		*dst = v
	default:
		d.bad(key, "an integer", v)
	}
}

func (d *decoder) int(key string, dst *int) {
	n := int64(*dst)
	d.int64(key, &n)
	*dst = int(n)
}

func (d *decoder) float(key string, dst *float64) {
	switch v := d.take(key).(type) {
	case nil:
	case int64:
		*dst = float64(v)
	case float64:
		*dst = v
	default:
		d.bad(key, "a number", v)
	}
}

func (d *decoder) str(key string, dst *string) {
	switch v := d.take(key).(type) {
	case nil:
	case string:
		*dst = v
	default:
		d.bad(key, "a string", v)
	}
}

func (d *decoder) bool(key string, dst *bool) {
	switch v := d.take(key).(type) {
	case nil:
	case bool:
		*dst = v
	default:
		d.bad(key, "true or false", v)
	}
}

func (d *decoder) dur(key string, dst *time.Duration) {
	switch v := d.take(key).(type) {
	case nil:
	case string:
		dur, err := time.ParseDuration(v)
		if err != nil {
			d.bad(key, "a duration such as 90s or 5m", v)
			return
		}
		*dst = dur
	default:
		d.bad(key, "a duration such as 90s or 5m", v)
	}
}

// size reads a byte size such as 2G or 512M.
func (d *decoder) size(key string, dst *int64) {
	v := d.take(key)
	if v == nil {
		return
	}
	n, err := api.ParseBytes(fmt.Sprint(v))
	if err != nil {
		d.bad(key, "a size such as 512M or 2G", v)
		return
	}
	*dst = n
}

func (d *decoder) finish() []string {
	keys := make([]string, 0, len(d.m))
	for k := range d.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d.errs = append(d.errs, fmt.Sprintf("%s%s: unknown key", d.prefix, k))
	}
	return d.errs
}

// Extract writes the task's shell files (and the common ones) into dir and
// returns it. Agents run them from there.
func (sc *Scenario) Extract(dir string) error {
	if sc.Task == "" {
		return nil // no script, no task files
	}
	for _, src := range []string{"files/common", "files/" + sc.Task} {
		entries, err := files.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			data, err := files.ReadFile(src + "/" + e.Name())
			if err != nil {
				return err
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o755); err != nil {
				return err
			}
		}
	}
	return nil
}

// Fails reports whether the tests of pkg fail at depth. It is a pure
// function of the seed, so every run of a scenario fails the same way.
func (sc *Scenario) Fails(pkg string, depth int) bool {
	if sc.FailRate <= 0 || depth >= sc.Depth {
		return false // the deepest fixer always succeeds, so recursion ends
	}
	return sc.draw(pkg, fmt.Sprint(depth)) < sc.FailRate
}

// draw is a number in [0, 1) fixed by the seed, the package and a label.
func (sc *Scenario) draw(pkg, label string) float64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d/%s/%s", sc.Seed, pkg, label)
	return float64(h.Sum64()%10000) / 10000
}

// Unbounded reports whether the worker of pkg is one of the greedy ones.
func (sc *Scenario) Unbounded(pkg string) bool {
	return sc.UnboundedParallel && sc.draw(pkg, "g6") < sc.UnboundedShare
}

// TaskEnv is the environment the task's shell files see for one agent.
func (sc *Scenario) TaskEnv(task string, depth int) []string {
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	_, pkg, _ := strings.Cut(task, ":")
	env := []string{
		"PKG=" + pkg,
		fmt.Sprint("DEPTH=", depth),
		fmt.Sprint("SEED=", sc.Seed),
		fmt.Sprint("FANOUT=", sc.Fanout),
		fmt.Sprint("DURATION_S=", int(sc.Duration.Seconds())),
		"FAIL=" + b(sc.Fails(pkg, depth)),
		"G2_LEAVE_SERVERS=" + b(sc.LeaveServers),
		"G5_RUNAWAY=" + b(sc.OutputRunaway),
		"G6_UNBOUNDED=" + b(sc.Unbounded(pkg)),
	}
	if sc.MaxPackages > 0 {
		env = append(env, fmt.Sprint("MAX_PACKAGES=", sc.MaxPackages))
	}
	keys := make([]string, 0, len(sc.Env))
	for k := range sc.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+sc.Env[k])
	}
	return env
}

func bash(cmd string, timeout time.Duration) map[string]any {
	return map[string]any{"command": cmd, "timeout": timeout.Seconds()}
}

// Script returns the steps the scripted brain plays for a task at a depth.
//
//	""               the lead: list the packages, fan out, re-dispatch
//	upgrade:<pkg>    a worker: set up, type-check, test, preview
//	fix:<pkg>        a fixer spawned by a worker (or fixer) whose tests failed
//	runaway          the G5 worker: a command that writes without end
func (sc *Scenario) Script(task string, depth int) ([]llm.Step, error) {
	const step = 10 * time.Minute
	kind, pkg, _ := strings.Cut(task, ":")
	// A failing test hands the package to a fixer one level down.
	test := `"$TASK_DIR/test.sh"`
	if depth < sc.Depth {
		test += ` || "$MINI_AGENT" --task "fix:$PKG"`
	}
	switch kind {
	case "":
		left := max(sc.Duration-sc.FanoutAt, time.Minute)
		dispatch := bash(`"$TASK_DIR/dispatch.sh"`, left)
		return []llm.Step{
			{Say: "I'll read the package list first.", Tool: "bash", Input: bash(`"$TASK_DIR/list.sh" | head -n "${MAX_PACKAGES:-1000}"`, time.Minute)},
			{At: sc.FanoutAt, Say: "One worker per package, in parallel.", Tool: "bash", Input: dispatch},
			{Say: "Some workers did not report back. Dispatching those again.", Tool: "bash", Input: dispatch},
			{Say: "Checking once more for packages without a result.", Tool: "bash", Input: dispatch},
			{At: sc.Duration, Say: "Time is up. Collecting results.", Tool: "bash", Input: bash(`ls "$RUN_DIR/results" | wc -l`, time.Minute)},
		}, nil
	case "upgrade":
		if pkg == "" {
			return nil, fmt.Errorf("task %q names no package", task)
		}
		return []llm.Step{
			{Say: "Setting up a worktree for " + pkg + ".", Tool: "bash", Input: bash(`"$TASK_DIR/setup.sh"`, step)},
			{Say: "Type-checking.", Tool: "bash", Input: bash(`"$TASK_DIR/typecheck.sh"`, step)},
			{Say: "Starting the dev server to look at the stories.", Tool: "bash", Input: bash(`"$TASK_DIR/preview.sh"`, step)},
			{Say: "Running the tests and checking the stories in a browser.", Tool: "bash", Input: bash(test+` && echo ok > "$WORK/status.test"`, 2*step)},
			{Say: "Stopping the dev server.", Tool: "bash", Input: bash(`"$TASK_DIR/cleanup.sh"`, time.Minute)},
			{Say: "Wrapping up.", Tool: "bash", Input: bash(`"$TASK_DIR/finish.sh" setup typecheck preview test`, time.Minute)},
		}, nil
	case "fix":
		if pkg == "" {
			return nil, fmt.Errorf("task %q names no package", task)
		}
		return []llm.Step{
			{Say: "Looking at why the tests of " + pkg + " fail.", Tool: "bash", Input: bash(`"$TASK_DIR/fix.sh"`, step)},
			{Say: "Running the tests again.", Tool: "bash", Input: bash(test+` && echo ok > "$WORK/status.test"`, 2*step)},
			{Say: "Wrapping up.", Tool: "bash", Input: bash(`"$TASK_DIR/finish.sh" test`, time.Minute)},
		}, nil
	case "runaway":
		left := max(sc.Duration-sc.OutputRunawayAt, time.Minute)
		return []llm.Step{
			{At: sc.OutputRunawayAt, Say: "Installing dependencies.", Tool: "bash", Input: bash(`"$TASK_DIR/runaway.sh"`, left)},
		}, nil
	}
	return nil, fmt.Errorf("unknown task %q", task)
}
