package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Step is one scripted assistant turn: some text and at most one tool call.
type Step struct {
	// At delays the step until the scenario clock reaches it. Zero means
	// as soon as the previous step's tool has returned.
	At   time.Duration
	Say  string
	Tool string
	// Input is the tool's arguments; it is marshalled to JSON.
	Input map[string]any
}

// Scripted is a deterministic brain: it plays Steps in order, costs no
// tokens and needs no network. When the steps run out it answers with a
// final text message, which ends the agent loop.
type Scripted struct {
	Steps []Step
	// Clock returns the time since the scenario began. All agents of one
	// run share the same origin, so a script can be timed across processes.
	Clock func() time.Duration
	// QuotaAt and QuotaFor define the window in which every call fails with
	// ErrQuota, to inject one external abort deterministically. A zero
	// QuotaFor disables it.
	QuotaAt, QuotaFor time.Duration
	// LeakBytes is retained on every step and never released. It simulates
	// a transcript buffer that only grows (greedy behaviour G3).
	LeakBytes int64

	next int
	leak [][]byte
}

func (s *Scripted) inQuotaWindow(now time.Duration) bool {
	return s.QuotaFor > 0 && now >= s.QuotaAt && now < s.QuotaAt+s.QuotaFor
}

// Stream returns the next step. A call that fails with ErrQuota does not
// consume a step, so the caller may retry it after the window.
func (s *Scripted) Stream(ctx context.Context, _ Request, onText func(string)) (Message, error) {
	if s.next >= len(s.Steps) {
		return Message{Role: "assistant", Blocks: []Block{{Type: BlockText, Text: "Done."}}}, nil
	}
	st := s.Steps[s.next]
	for {
		now := s.Clock()
		if s.inQuotaWindow(now) {
			return Message{}, ErrQuota
		}
		wait := st.At - now
		if wait <= 0 {
			break
		}
		// Wake at the quota window if it opens before the step is due.
		if s.QuotaFor > 0 && now < s.QuotaAt && s.QuotaAt-now < wait {
			wait = s.QuotaAt - now
		}
		select {
		case <-ctx.Done():
			return Message{}, ctx.Err()
		case <-time.After(wait):
		}
	}
	if s.LeakBytes > 0 {
		buf := make([]byte, s.LeakBytes)
		for i := 0; i < len(buf); i += 4096 {
			buf[i] = 1 // touch every page so the memory is really used
		}
		s.leak = append(s.leak, buf)
	}
	s.next++
	msg := Message{Role: "assistant"}
	if st.Say != "" {
		if onText != nil {
			onText(st.Say)
		}
		msg.Blocks = append(msg.Blocks, Block{Type: BlockText, Text: st.Say})
	}
	if st.Tool != "" {
		in, err := json.Marshal(st.Input)
		if err != nil {
			return Message{}, fmt.Errorf("scripted step %d: %w", s.next, err)
		}
		msg.Blocks = append(msg.Blocks, Block{Type: BlockToolUse, ID: fmt.Sprintf("toolu_%04d", s.next), Name: st.Tool, Input: in})
	}
	return msg, nil
}
