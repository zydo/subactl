package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zydo/subactl/examples/mini-agent/agent"
)

// runToy builds mini-agent, runs the toy task in without mode and returns
// the results and how many "dev server" and "browser" processes outlived
// the run.
func runToy(t *testing.T, scenario string) (results map[string]string, leftover int) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "mini-agent")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	sc := filepath.Join(dir, "scenario.yaml")
	if err := os.WriteFile(sc, []byte(scenario), 0o644); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(dir, fmt.Sprintf("run%d", os.Getpid()))
	marker := "toy-(server|chrome)-" + filepath.Base(runDir) + "-"
	t.Cleanup(func() { exec.Command("pkill", "-f", marker).Run() })

	cmd := exec.Command(bin, "--mode", "without", "--scenario", sc, "--run-dir", runDir)
	cmd.Env = append(os.Environ(), "SUBACTL_EXPERIMENT=", "MINI_AGENT_DEPTH=", "MINI_AGENT_RUN=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mini-agent: %v\n%s", err, out)
	}
	results = map[string]string{}
	for _, r := range (agent.Registry{Dir: runDir}).Results() {
		results[r.Task] = r.Status
	}
	pg, _ := exec.Command("pgrep", "-f", marker).Output()
	leftover = len(strings.Fields(string(pg)))
	if t.Failed() || testing.Verbose() {
		t.Logf("output:\n%s", out)
	}
	return results, leftover
}

func wantAllOK(t *testing.T, results map[string]string, packages int) {
	t.Helper()
	for i := 1; i <= packages; i++ {
		task := fmt.Sprintf("upgrade:pkg%d.d1", i)
		if results[task] != "ok" {
			b, _ := json.Marshal(results)
			t.Fatalf("%s is %q; results: %s", task, results[task], b)
		}
	}
}

// A well-behaved run completes every package, recursing into fixers where
// tests fail, and leaves nothing behind.
func TestToyClean(t *testing.T) {
	results, leftover := runToy(t, `
seed: 7
task: toy
fanout: 6
depth: 3
fail_rate: 0.5
duration: 4s
fanout_at: 200ms
env:
  TOY_PACKAGES: 6
  TOY_STEP: 0.1
  TOY_SERVER_LIFE: 60
`)
	wantAllOK(t, results, 6)
	fixers := 0
	for task := range results {
		if strings.HasPrefix(task, "fix:") {
			fixers++
		}
	}
	if fixers == 0 {
		t.Error("fail_rate 0.5 produced no fixer; recursion is not exercised")
	}
	if leftover != 0 {
		t.Errorf("%d processes left behind by a clean run", leftover)
	}
}

// G4, the defect the design sets out to contain: at the usage limit the
// harness kills each subagent's pid and nothing else, so the dev servers
// and browsers those subagents had started become orphans. The lead then
// dispatches the lost packages again and they complete.
func TestToyQuotaKillLeavesOrphans(t *testing.T) {
	results, leftover := runToy(t, `
seed: 7
task: toy
fanout: 6
depth: 1
duration: 9s
fanout_at: 500ms
quota_at: 2750ms
quota_for: 1s
env:
  TOY_PACKAGES: 6
  TOY_STEP: 0.5
  TOY_SERVER_LIFE: 60
`)
	wantAllOK(t, results, 6)
	if leftover == 0 {
		t.Fatal("no orphans after the subagents were killed; the baseline defect is not reproduced")
	}
	t.Logf("%d orphaned processes after the usage limit", leftover)
}
