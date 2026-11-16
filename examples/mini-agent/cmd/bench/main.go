// Command bench runs the controlled experiments: a scenario, several
// times, each in a fresh blast chamber, with metrics collected per run.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/examples/mini-agent/bench"
	"github.com/zydo/subactl/examples/mini-agent/scenarios"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: bench run [flags] | bench report --in DIR --out DIR | bench extract TASK DIR | bench hog [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = run(os.Args[2:])
	case "extract":
		// Write a task's shell files to a directory, e.g. to run prepare.sh.
		if len(os.Args) != 4 {
			err = fmt.Errorf("usage: bench extract TASK DIR")
			break
		}
		err = (&scenarios.Scenario{Task: os.Args[2]}).Extract(os.Args[3])
	case "hog":
		err = hog(os.Args[2:])
	case "report":
		err = report(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q: want run or hog", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func hog(args []string) error {
	fs := flag.NewFlagSet("hog", flag.ExitOnError)
	bytes := fs.Int64("bytes", 0, "bytes to hold")
	ramp := fs.Duration("ramp", 30*time.Second, "time to reach the full size")
	hold := fs.Duration("hold", 4*time.Minute, "time to hold it")
	fs.Parse(args)
	bench.Hog(*bytes, *ramp, *hold)
	return nil
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	group := fs.String("group", "A", "experiment group: A without subactl, B with static envelopes, C with suggested envelopes that grow")
	reserveFlag := fs.String("reserve", "", "reserve line inside the chamber for groups B and C (default: 15% of the chamber)")
	scenarioPath := fs.String("scenario", "", "scenario file")
	runs := fs.Int("runs", 5, "how many runs")
	out := fs.String("out", "bench-out", "output directory")
	workDir := fs.String("workdir", ".", "working directory of the lead, e.g. the radix checkout")
	chamber := fs.String("chamber", "12G", "memory cap of the blast chamber")
	base := fs.String("cgroup", "/sys/fs/cgroup/subactl-bench", "parent cgroup of the chambers")
	userName := fs.String("user", os.Getenv("SUDO_USER"), "user the lead runs as; never root")
	miniAgent := fs.String("mini-agent", "", "mini-agent binary (default: next to this binary)")
	settle := fs.Duration("settle", 60*time.Second, "how long to observe after the lead ends")
	fs.Parse(args)

	if *group != "A" && *group != "B" && *group != "C" {
		return fmt.Errorf("unknown group %q: want A, B or C", *group)
	}
	sc, err := scenarios.Load(*scenarioPath)
	if err != nil {
		return err
	}
	bytes, err := api.ParseBytes(*chamber)
	if err != nil {
		return fmt.Errorf("--chamber: %w", err)
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("bench run must be root to create the chamber cgroup")
	}
	u, err := user.Lookup(*userName)
	if err != nil || u.Uid == "0" {
		return fmt.Errorf("--user %q: name a non-root user for the lead to run as", *userName)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if *miniAgent == "" {
		*miniAgent = filepath.Join(filepath.Dir(self), "mini-agent")
	}
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }
	reserve := bytes * 15 / 100
	if *reserveFlag != "" {
		if reserve, err = api.ParseBytes(*reserveFlag); err != nil {
			return fmt.Errorf("--reserve: %w", err)
		}
	}
	grp, err := user.LookupGroupId(u.Gid)
	if err != nil {
		return err
	}
	mode := "without"
	if *group != "A" {
		mode = "with"
	}

	var all []*bench.Summary
	for i := 1; i <= *runs; i++ {
		outDir := filepath.Join(abs(*out), *group, fmt.Sprintf("run-%d", i))
		if err := os.RemoveAll(outDir); err != nil {
			return err
		}
		sum, err := bench.Run(bench.Options{
			Group: *group, RunIndex: i, ScenarioPath: abs(*scenarioPath), Scenario: sc, Mode: mode,
			BinDir: filepath.Dir(self), Reserve: reserve, SocketGroup: grp.Name, Adaptive: *group == "C",
			StateDir:  stateDir(abs(*out), *group, i),
			MiniAgent: abs(*miniAgent), Self: self, OutDir: outDir, WorkDir: abs(*workDir),
			Base: *base, ChamberBytes: bytes, UID: uid, GID: gid, Home: u.HomeDir, Settle: *settle,
			Log: func(f string, a ...any) { fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05 ")+f+"\n", a...) },
		})
		if err != nil {
			return fmt.Errorf("run %d: %w", i, err)
		}
		all = append(all, sum)
		line, _ := json.Marshal(sum)
		fmt.Println(string(line))
	}
	data, _ := json.MarshalIndent(all, "", "  ")
	return os.WriteFile(filepath.Join(abs(*out), *group, "summary.json"), data, 0o644)
}

// stateDir is where subactld keeps its audit log. Group C keeps one
// directory for all its runs: its suggestions are meant to improve from
// run to run. Group B starts every run without history.
func stateDir(out, group string, run int) string {
	if group == "C" {
		return filepath.Join(out, group, "state")
	}
	return filepath.Join(out, group, fmt.Sprintf("run-%d", run), "state")
}

// report turns the measurements of the groups into the report: verdicts
// on the hypotheses, tables, charts and the raw series as Parquet.
func report(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	in := fs.String("in", "", "directory holding the groups' results (A, B, C)")
	out := fs.String("out", "", "directory to write the report into (default: the input directory)")
	fs.Parse(args)
	if *in == "" {
		return fmt.Errorf("--in is required")
	}
	if *out == "" {
		*out = *in
	}
	groups, err := bench.LoadResults(*in)
	if err != nil {
		return err
	}
	verdicts, err := bench.WriteReport(*out, groups)
	if err != nil {
		return err
	}
	for _, v := range verdicts {
		fmt.Printf("%s %s: %s\n", v.ID, v, v.Evidence)
	}
	return nil
}
