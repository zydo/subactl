// Package llm defines the brain of mini-agent: one streaming call. The
// scripted brain implements it directly; the Anthropic brain lands in M5.
package llm

import (
	"context"
	"encoding/json"
	"errors"
)

// Block types.
const (
	BlockText       = "text"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
)

// Block is one piece of a message.
type Block struct {
	Type string `json:"type"`
	// Text holds the text of a text block or the output of a tool result.
	Text string `json:"text,omitempty"`
	// ID and Name identify a tool call; Input is its JSON arguments.
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// ToolUseID links a tool result to its call.
	ToolUseID string `json:"tool_use_id,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// Message is one turn. Role is "user" or "assistant".
type Message struct {
	Role   string  `json:"role"`
	Blocks []Block `json:"content"`
}

// ToolCalls returns the tool_use blocks of a message.
func (m Message) ToolCalls() []Block {
	var out []Block
	for _, b := range m.Blocks {
		if b.Type == BlockToolUse {
			out = append(out, b)
		}
	}
	return out
}

// Tool describes a tool to the brain.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"input_schema"`
}

// Request is everything a brain sees.
type Request struct {
	System   string
	Messages []Message
	Tools    []Tool
}

// Brain produces the next assistant message. onText receives text as it
// streams and may be nil.
type Brain interface {
	Stream(ctx context.Context, req Request, onText func(string)) (Message, error)
}

var (
	// ErrQuota means the account's usage limit was reached. It is the
	// external abort of the design: nothing is wrong with the task.
	ErrQuota = errors.New("llm: usage limit reached")
	// ErrContextOverflow means the conversation no longer fits the model's
	// context window. mini-agent does not compact; the worker gives up.
	ErrContextOverflow = errors.New("llm: context window exceeded")
)
