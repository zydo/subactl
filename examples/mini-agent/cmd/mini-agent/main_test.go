package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// The workers start their servers about 2s in and keep them through a 4s
// verification step; the limit hits at 4s, in the middle of that window
// with two seconds to spare on either side for a slow machine.
//
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
duration: 13s
fanout_at: 500ms
quota_at: 4s
quota_for: 1s
env:
  TOY_PACKAGES: 6
  TOY_STEP: 0.5
  TOY_TEST_STEP: 4
  TOY_SERVER_LIFE: 60
`)
	wantAllOK(t, results, 6)
	if leftover == 0 {
		t.Fatal("no orphans after the subagents were killed; the baseline defect is not reproduced")
	}
	t.Logf("%d orphaned processes after the usage limit", leftover)
}

// With a real provider, a usage limit ends the turn after one request,
// says so, and leaves the lead at its prompt; it does not ask again and
// again. The provider here is a stand-in that always answers 429.
func TestRealBrainUsageLimit(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"Usage limit reached for 5 hour"}}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	bin := filepath.Join(dir, "mini-agent")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "--mode", "without", "--brain", "real", "--provider", "deepseek", "--base-url", srv.URL, "--run-dir", filepath.Join(dir, "run"))
	cmd.Env = append(os.Environ(), "DEEPSEEK_API_KEY=test", "MINI_AGENT_DEPTH=", "MINI_AGENT_RUN=")
	stdin, _ := cmd.StdinPipe()
	var out lockedOutput
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	io.WriteString(stdin, "say hi\n")
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "usage limit was reached") {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("no report of the limit:\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(2 * time.Second) // time for any retry to show
	stdin.Close()               // end of input: the lead ends
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the lead did not end cleanly: %v\n%s", err, out.String())
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("%d requests to a provider at its limit, want 1", n)
	}
	// After the report the lead prompts again, and only stops when its
	// input ends.
	if after, ok := strings.CutPrefix(out.String()[strings.Index(out.String(), "usage limit was reached"):], "usage limit was reached"); !ok || !strings.Contains(after, "\n> ") {
		t.Fatalf("the lead did not return to its prompt:\n%s", out.String())
	}
}

type lockedOutput struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedOutput) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedOutput) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
