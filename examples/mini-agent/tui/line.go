// Package tui is mini-agent's line-mode output: events are appended one
// line at a time. There is no full-screen layout to keep in sync.
package tui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/zydo/subactl/examples/mini-agent/agent"
)

// Printer writes agent events as lines.
type Printer struct {
	mu    sync.Mutex
	out   []io.Writer
	label string
	clock func() time.Duration
}

// NewPrinter prints to every writer, prefixing each line with the scenario
// time and label.
func NewPrinter(label string, clock func() time.Duration, out ...io.Writer) *Printer {
	return &Printer{out: out, label: label, clock: clock}
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// Event prints one event.
func (p *Printer) Event(e agent.Event) {
	var line string
	switch e.Kind {
	case agent.EventText:
		line = e.Text
	case agent.EventToolCall:
		line = fmt.Sprintf("→ %s #%d %s", e.Tool, e.Seq, oneLine(e.Text, 160))
	case agent.EventToolResult:
		mark := "←"
		if e.IsError {
			mark = "✗"
		}
		line = fmt.Sprintf("%s %s #%d %s", mark, e.Tool, e.Seq, oneLine(e.Text, 200))
	case agent.EventSteer:
		line = "⇢ " + oneLine(e.Text, 200)
	case agent.EventQuota:
		line = "! " + e.Text
	default:
		return
	}
	p.Line(line)
}

// Line prints one line.
func (p *Printer) Line(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.clock().Round(time.Second)
	for _, w := range p.out {
		fmt.Fprintf(w, "%6s [%s] %s\n", t, p.label, s)
	}
}
