package scenarios

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const full = `
seed: 42
task: toy
fanout: 32
depth: 3
max_packages: 10
leak_mb: 8
fail_rate: 0.3
duration: 15m
fanout_at: 2m
quota_at: 5m
quota_for: 20s
output_runaway_at: 9m
greedy:
  leave_servers: true
  output_runaway: true
  unbounded_parallel: true
env:
  TOY_PACKAGES: 6
`

func TestParse(t *testing.T) {
	sc, err := Parse([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Seed != 42 || sc.Fanout != 32 || sc.Depth != 3 || sc.LeakMB != 8 || sc.FailRate != 0.3 ||
		sc.Duration != 15*time.Minute || sc.QuotaAt != 5*time.Minute || sc.QuotaFor != 20*time.Second ||
		!sc.LeaveServers || !sc.OutputRunaway || !sc.UnboundedParallel || sc.Env["TOY_PACKAGES"] != "6" {
		t.Fatalf("got %+v", sc)
	}
	for name, src := range map[string]string{
		"typo":          "duration: 1m\nfan_out: 3",
		"greedy typo":   "duration: 1m\ngreedy:\n  leave_server: true",
		"bad duration":  "duration: soon",
		"no duration":   "seed: 1",
		"unknown task":  "duration: 1m\ntask: nope",
		"bad fail rate": "duration: 1m\nfail_rate: 2",
		"wrong type":    "duration: 1m\nfanout: many",
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFailsIsDeterministicAndBounded(t *testing.T) {
	sc, _ := Parse([]byte(full))
	failed := 0
	for i := range 200 {
		pkg := "pkg" + strings.Repeat("x", i)
		first, again := sc.Fails(pkg, 1), sc.Fails(pkg, 1)
		if first != again {
			t.Fatal("not deterministic")
		}
		if sc.Fails(pkg, 1) {
			failed++
		}
		if sc.Fails(pkg, sc.Depth) {
			t.Fatal("the deepest level must not fail, or recursion never ends")
		}
	}
	if failed < 30 || failed > 90 { // 30% of 200, loosely
		t.Fatalf("%d of 200 failed at rate 0.3", failed)
	}
}

func TestScriptsAndFiles(t *testing.T) {
	sc, _ := Parse([]byte(full))
	dir := t.TempDir()
	if err := sc.Extract(dir); err != nil {
		t.Fatal(err)
	}
	// Every shell file a script names must have been extracted, executable.
	for _, tc := range []struct {
		task  string
		depth int
	}{{"", 0}, {"upgrade:dialog", 1}, {"fix:dialog", 2}, {"fix:dialog", 3}, {"runaway", 1}} {
		steps, err := sc.Script(tc.task, tc.depth)
		if err != nil || len(steps) == 0 {
			t.Fatalf("%q: %v", tc.task, err)
		}
		for _, st := range steps {
			cmd, _ := st.Input["command"].(string)
			for _, word := range strings.Fields(cmd) {
				name, ok := strings.CutPrefix(strings.Trim(word, `"`), "$TASK_DIR/")
				if !ok {
					continue
				}
				info, err := os.Stat(filepath.Join(dir, name))
				if err != nil || info.Mode()&0o111 == 0 {
					t.Errorf("%q uses %s, which is missing or not executable", tc.task, name)
				}
			}
			spawns := strings.Contains(cmd, `"$MINI_AGENT" --task "fix:`)
			if strings.Contains(cmd, "test.sh") && spawns != (tc.depth < sc.Depth) {
				t.Errorf("%q at depth %d: fixer spawn is %v", tc.task, tc.depth, spawns)
			}
		}
	}
	if _, err := sc.Script("upgrade:", 1); err == nil {
		t.Error("a task without a package was accepted")
	}
	if _, err := sc.Script("dance", 1); err == nil {
		t.Error("an unknown task was accepted")
	}
}

// The scenario files shipped with mini-agent must parse.
func TestShippedScenarios(t *testing.T) {
	files, _ := filepath.Glob("*.yaml")
	if len(files) < 3 {
		t.Fatalf("found %d scenario files", len(files))
	}
	for _, f := range files {
		if _, err := Load(f); err != nil {
			t.Error(err)
		}
	}
}
