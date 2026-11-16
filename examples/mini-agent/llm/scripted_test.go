package llm

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestScripted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		s := &Scripted{
			Clock:   func() time.Duration { return time.Since(start) },
			QuotaAt: 5 * time.Minute, QuotaFor: 30 * time.Second, LeakBytes: 1 << 20,
			Steps: []Step{
				{Say: "first", Tool: "bash", Input: map[string]any{"command": "true"}},
				{At: 2 * time.Minute, Tool: "bash", Input: map[string]any{"command": "true"}},
				{At: 9 * time.Minute, Say: "late"},
			},
		}
		ctx := context.Background()
		var said string
		m, err := s.Stream(ctx, Request{}, func(x string) { said += x })
		if err != nil || said != "first" || len(m.ToolCalls()) != 1 || m.ToolCalls()[0].Name != "bash" {
			t.Fatalf("step 1: %+v, %v", m, err)
		}
		// A timed step waits for the scenario clock.
		if _, err := s.Stream(ctx, Request{}, nil); err != nil || time.Since(start) != 2*time.Minute {
			t.Fatalf("step 2 at %v: %v", time.Since(start), err)
		}
		// Waiting for a later step is interrupted by the usage limit, and
		// the failed call does not consume the step.
		if _, err := s.Stream(ctx, Request{}, nil); !errors.Is(err, ErrQuota) || time.Since(start) != 5*time.Minute {
			t.Fatalf("want quota at 5m, got %v at %v", err, time.Since(start))
		}
		if _, err := s.Stream(ctx, Request{}, nil); !errors.Is(err, ErrQuota) {
			t.Fatalf("still inside the window: %v", err)
		}
		time.Sleep(31 * time.Second)
		m, err = s.Stream(ctx, Request{}, nil)
		if err != nil || time.Since(start) != 9*time.Minute || len(m.ToolCalls()) != 0 {
			t.Fatalf("step 3 at %v: %+v, %v", time.Since(start), m, err)
		}
		// Out of steps: a final message without a tool call.
		if m, err := s.Stream(ctx, Request{}, nil); err != nil || len(m.ToolCalls()) != 0 || len(m.Blocks) != 1 {
			t.Fatalf("after the script: %+v, %v", m, err)
		}
		if len(s.leak) != 3 {
			t.Fatalf("leaked %d buffers, want one per step", len(s.leak))
		}
	})
}
