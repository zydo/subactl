// Package agent is the mini-agent loop: call the brain, run the tools it
// asks for one after another, feed the results back, repeat.
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zydo/subactl/examples/mini-agent/llm"
	"github.com/zydo/subactl/examples/mini-agent/tools"
)

// Event kinds.
const (
	EventText       = "text"
	EventToolCall   = "tool_call"
	EventToolResult = "tool_result"
	EventSteer      = "steer"
	EventQuota      = "quota"
)

// Event is something that happened in the loop, for the TUI and logs.
type Event struct {
	Kind    string
	Text    string
	Tool    string
	Seq     int // tool-call sequence number, 1-based
	IsError bool
}

// ErrStop is returned by a BeforeTool hook to end the run without running
// the tool, e.g. after TERMINATE_REQUESTED.
var ErrStop = errors.New("agent: stopped by hook")

// Agent runs one conversation.
type Agent struct {
	Brain  llm.Brain
	Tools  []tools.Tool
	System string
	// OnEvent receives every event; it may be nil.
	OnEvent func(Event)
	// BeforeTool runs before each tool call; returning ErrStop ends the run.
	BeforeTool func(ctx context.Context, tool string, seq int) error
	// AfterTurn runs after each turn's tool calls, before the next brain call.
	AfterTurn func(ctx context.Context, turn int)
	// OnQuota runs when the brain reports the usage limit. It returns how
	// long to wait before retrying, or false to give up.
	OnQuota func(ctx context.Context) (retryAfter time.Duration, retry bool)

	mu       sync.Mutex
	steer    []string
	messages []llm.Message
	toolSeq  int
	tokens   int64
}

// Tokens is what the brain reported using so far. It is self-reported.
func (a *Agent) Tokens() int64 { return a.tokens }

// Steer queues a message that is injected after the current turn's tool
// calls, as a user message.
func (a *Agent) Steer(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steer = append(a.steer, text)
}

func (a *Agent) takeSteer() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.steer
	a.steer = nil
	return s
}

func (a *Agent) emit(e Event) {
	if a.OnEvent != nil {
		a.OnEvent(e)
	}
}

// Transcript returns the conversation so far.
func (a *Agent) Transcript() []llm.Message { return a.messages }

// Run plays the conversation from prompt until the brain answers without a
// tool call. Cancelling ctx aborts the current turn.
func (a *Agent) Run(ctx context.Context, prompt string) error {
	specs := make([]llm.Tool, len(a.Tools))
	byName := map[string]tools.Tool{}
	for i, t := range a.Tools {
		specs[i] = t.Spec()
		byName[specs[i].Name] = t
	}
	a.messages = append(a.messages, userText(prompt))
	for turn := 1; ; turn++ {
		// Line mode shows a message's text when it is complete, not as it
		// streams.
		msg, err := a.Brain.Stream(ctx, llm.Request{System: a.System, Messages: a.messages, Tools: specs}, nil)
		if errors.Is(err, llm.ErrQuota) {
			a.emit(Event{Kind: EventQuota, Text: err.Error(), IsError: true})
			if a.OnQuota != nil {
				if after, retry := a.OnQuota(ctx); retry {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(after):
					}
					turn--
					continue
				}
			}
			return err
		}
		if err != nil {
			return err
		}
		a.messages = append(a.messages, msg)
		a.tokens += msg.Tokens
		for _, b := range msg.Blocks {
			if b.Type == llm.BlockText && strings.TrimSpace(b.Text) != "" {
				a.emit(Event{Kind: EventText, Text: strings.TrimSpace(b.Text)})
			}
		}
		calls := msg.ToolCalls()
		if len(calls) == 0 {
			return nil
		}
		results := llm.Message{Role: "user"}
		for _, call := range calls {
			a.toolSeq++
			if a.BeforeTool != nil {
				if err := a.BeforeTool(ctx, call.Name, a.toolSeq); err != nil {
					return err
				}
			}
			a.emit(Event{Kind: EventToolCall, Tool: call.Name, Seq: a.toolSeq, Text: string(call.Input)})
			var out string
			var terr error
			if t, ok := byName[call.Name]; ok {
				out, terr = t.Run(ctx, call.Input)
			} else {
				terr = fmt.Errorf("unknown tool %q; the tools are read, write, edit and bash", call.Name)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if terr != nil {
				out = terr.Error()
			}
			a.emit(Event{Kind: EventToolResult, Tool: call.Name, Seq: a.toolSeq, Text: out, IsError: terr != nil})
			results.Blocks = append(results.Blocks, llm.Block{Type: llm.BlockToolResult, ToolUseID: call.ID, Text: out, IsError: terr != nil})
		}
		// Steered messages ride along with the tool results.
		for _, s := range a.takeSteer() {
			a.emit(Event{Kind: EventSteer, Text: s})
			results.Blocks = append(results.Blocks, llm.Block{Type: llm.BlockText, Text: s})
		}
		a.messages = append(a.messages, results)
		if a.AfterTurn != nil {
			a.AfterTurn(ctx, turn)
		}
	}
}

func userText(s string) llm.Message {
	return llm.Message{Role: "user", Blocks: []llm.Block{{Type: llm.BlockText, Text: s}}}
}
