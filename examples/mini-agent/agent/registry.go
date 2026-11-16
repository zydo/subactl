package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Registry is the run directory shared by all agents of one run. It is how
// a harness knows its subagents and how results travel back to a planner.
//
//	<dir>/agents/<pid>.json    one file per live agent
//	<dir>/results/<task>.json  one file per finished task
//	<dir>/work/<task>/         the agent's scratch: tool output spills
type Registry struct {
	Dir string
}

// Entry describes one live agent.
type Entry struct {
	Pid    int    `json:"pid"`
	Parent int    `json:"parent"` // pid of the parent agent, 0 for the lead
	Depth  int    `json:"depth"`
	Task   string `json:"task"`
}

// Result is what a subagent hands back to its planner.
type Result struct {
	Task   string `json:"task"`
	Status string `json:"status"` // "ok" or "failed"
	Detail string `json:"detail,omitempty"`
}

// TaskFile turns a task name into a safe file name.
func TaskFile(task string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, task)
}

func (r Registry) agents() string { return filepath.Join(r.Dir, "agents") }

// WorkDir is the scratch directory of a task.
func (r Registry) WorkDir(task string) string { return filepath.Join(r.Dir, "work", TaskFile(task)) }

// ResultPath is where the result of a task is written.
func (r Registry) ResultPath(task string) string {
	return filepath.Join(r.Dir, "results", TaskFile(task)+".json")
}

// Register records this process as a live agent.
func (r Registry) Register(e Entry) error {
	for _, d := range []string{r.agents(), filepath.Join(r.Dir, "results"), r.WorkDir(e.Task)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	data, _ := json.Marshal(e)
	return os.WriteFile(filepath.Join(r.agents(), strconv.Itoa(e.Pid)+".json"), data, 0o644)
}

// Unregister removes this process's entry. An agent that is killed never
// gets to call it; stale entries are skipped by Children.
func (r Registry) Unregister(pid int) {
	_ = os.Remove(filepath.Join(r.agents(), strconv.Itoa(pid)+".json"))
}

// Children returns the live agents whose parent is pid.
func (r Registry) Children(pid int) []Entry {
	files, _ := filepath.Glob(filepath.Join(r.agents(), "*.json"))
	var out []Entry
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var e Entry
		if json.Unmarshal(data, &e) != nil || e.Parent != pid {
			continue
		}
		if syscall.Kill(e.Pid, 0) != nil { // gone
			continue
		}
		out = append(out, e)
	}
	return out
}

// KillChildren sends SIGKILL to the pid of every direct subagent, and to
// nothing else: not its process group, not its descendants. This is what a
// harness does when a subagent hits the usage limit, and it is the defect
// the design sets out to contain: whatever the subagent started is left
// running. With subactl the same kill makes subactld reap the whole
// acgroup, so the harness code is identical in both modes.
func (r Registry) KillChildren(pid int) []Entry {
	kids := r.Children(pid)
	for _, k := range kids {
		_ = syscall.Kill(k.Pid, syscall.SIGKILL)
		r.Unregister(k.Pid)
	}
	return kids
}

// WriteResult records the outcome of a task.
func (r Registry) WriteResult(res Result) error {
	data, _ := json.Marshal(res)
	tmp := r.ResultPath(res.Task) + fmt.Sprintf(".%d.tmp", os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.ResultPath(res.Task))
}

// Results reads every result written so far.
func (r Registry) Results() []Result {
	files, _ := filepath.Glob(filepath.Join(r.Dir, "results", "*.json"))
	var out []Result
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var res Result
		if json.Unmarshal(data, &res) == nil {
			out = append(out, res)
		}
	}
	return out
}
