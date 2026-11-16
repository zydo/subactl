package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/zydo/subactl/examples/mini-agent/llm"
	"github.com/zydo/subactl/examples/mini-agent/tools"
)

type echo struct{ calls []string }

func (e *echo) Spec() llm.Tool { return llm.Tool{Name: "echo"} }
func (e *echo) Run(_ context.Context, in json.RawMessage) (string, error) {
	e.calls = append(e.calls, string(in))
	if strings.Contains(string(in), "boom") {
		return "", errors.New("it broke")
	}
	return "said " + string(in), nil
}

func scripted(steps ...llm.Step) *llm.Scripted {
	start := time.Now()
	return &llm.Scripted{Steps: steps, Clock: func() time.Duration { return time.Since(start) }}
}

func TestLoop(t *testing.T) {
	tool := &echo{}
	var kinds []string
	a := &Agent{
		Brain: scripted(
			llm.Step{Say: "one", Tool: "echo", Input: map[string]any{"x": 1}},
			llm.Step{Tool: "echo", Input: map[string]any{"x": "boom"}},
			llm.Step{Tool: "nope", Input: map[string]any{}},
		),
		Tools:   []tools.Tool{tool},
		OnEvent: func(e Event) { kinds = append(kinds, e.Kind) },
	}
	a.Steer("a note from the planner")
	if err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(tool.calls) != 2 {
		t.Fatalf("tool ran %d times", len(tool.calls))
	}
	// prompt, then (assistant, results) x3, then the final assistant text.
	msgs := a.Transcript()
	if len(msgs) != 8 {
		t.Fatalf("%d messages", len(msgs))
	}
	first := msgs[2].Blocks
	if len(first) != 2 || first[0].Type != llm.BlockToolResult || first[0].IsError || first[1].Text != "a note from the planner" {
		t.Fatalf("first results %+v", first)
	}
	if r := msgs[4].Blocks[0]; !r.IsError || r.Text != "it broke" {
		t.Fatalf("failed tool result %+v", r)
	}
	if r := msgs[6].Blocks[0]; !r.IsError || !strings.Contains(r.Text, "unknown tool") {
		t.Fatalf("unknown tool result %+v", r)
	}
	if got := strings.Join(kinds, ","); !strings.Contains(got, "text,tool_call,tool_result,steer") {
		t.Fatalf("events %s", got)
	}
}

func TestBeforeToolStops(t *testing.T) {
	tool := &echo{}
	a := &Agent{
		Brain: scripted(llm.Step{Tool: "echo", Input: map[string]any{}}, llm.Step{Tool: "echo", Input: map[string]any{}}),
		Tools: []tools.Tool{tool},
		BeforeTool: func(_ context.Context, _ string, seq int) error {
			if seq == 2 {
				return ErrStop
			}
			return nil
		},
	}
	if err := a.Run(context.Background(), "go"); !errors.Is(err, ErrStop) || len(tool.calls) != 1 {
		t.Fatalf("err %v after %d calls", err, len(tool.calls))
	}
}

type flaky struct{ fails int }

func (f *flaky) Stream(context.Context, llm.Request, func(string)) (llm.Message, error) {
	if f.fails > 0 {
		f.fails--
		return llm.Message{}, llm.ErrQuota
	}
	return llm.Message{Role: "assistant", Blocks: []llm.Block{{Type: llm.BlockText, Text: "ok"}}}, nil
}

func TestQuota(t *testing.T) {
	hooks := 0
	a := &Agent{Brain: &flaky{fails: 2}, OnQuota: func(context.Context) (time.Duration, bool) { hooks++; return time.Millisecond, true }}
	if err := a.Run(context.Background(), "go"); err != nil || hooks != 2 {
		t.Fatalf("err %v, %d hooks", err, hooks)
	}
	b := &Agent{Brain: &flaky{fails: 1}, OnQuota: func(context.Context) (time.Duration, bool) { return 0, false }}
	if err := b.Run(context.Background(), "go"); !errors.Is(err, llm.ErrQuota) {
		t.Fatalf("err %v", err)
	}
}

// KillChildren kills the registered subagent and nothing else.
func TestRegistryKillChildren(t *testing.T) {
	reg := Registry{Dir: t.TempDir()}
	me := os.Getpid()
	mine := exec.Command("sleep", "30")
	other := exec.Command("sleep", "30")
	for _, c := range []*exec.Cmd{mine, other} {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		defer c.Process.Kill()
	}
	reg.Register(Entry{Pid: mine.Process.Pid, Parent: me, Depth: 1, Task: "upgrade:a.d1"})
	reg.Register(Entry{Pid: other.Process.Pid, Parent: me + 1, Depth: 1, Task: "upgrade:b.d1"})
	reg.Register(Entry{Pid: 1 << 30, Parent: me, Depth: 1, Task: "gone"}) // stale entry

	killed := reg.KillChildren(me)
	if len(killed) != 1 || killed[0].Pid != mine.Process.Pid {
		t.Fatalf("killed %+v", killed)
	}
	if err := mine.Wait(); err == nil {
		t.Fatal("subagent survived")
	}
	if len(reg.Children(me+1)) != 1 {
		t.Fatal("another agent's subagent was touched")
	}
	if err := reg.WriteResult(Result{Task: "upgrade:a.d1", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	if rs := reg.Results(); len(rs) != 1 || rs[0].Status != "ok" {
		t.Fatalf("results %+v", rs)
	}
}
