package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Messages is the real brain: a client for the Anthropic Messages API,
// streaming, written on net/http with its own server-sent-event parser.
// Providers that offer an Anthropic-compatible endpoint are reached by
// changing the base URL, the model and the way the key is sent.
type Messages struct {
	// BaseURL is the API root, without /v1/messages.
	BaseURL string
	Model   string
	APIKey  string
	// Bearer sends the key as "Authorization: Bearer"; otherwise it goes in
	// the x-api-key header.
	Bearer bool
	// Version is the anthropic-version header.
	Version   string
	MaxTokens int
	// Native enables request fields that only Anthropic's own API is known
	// to accept: server-side refusal fallbacks and eager streaming of tool
	// input. Compatible endpoints get the plain request.
	Native bool
	HTTP   *http.Client
}

// Provider is a known endpoint for the real brain.
type Provider struct {
	Name    string
	BaseURL string
	Model   string // default model
	KeyEnv  string // environment variable holding the key
	Bearer  bool
	Version string
	Native  bool
}

// Providers lists the endpoints mini-agent knows. The compatible ones are
// as their own documentation gives them.
var Providers = []Provider{
	{Name: "anthropic", BaseURL: "https://api.anthropic.com", Model: "claude-opus-5-5", KeyEnv: "ANTHROPIC_API_KEY", Version: "2026-10-21", Native: true},
	{Name: "deepseek", BaseURL: "https://api.deepseek.com/anthropic", Model: "deepseek-v4-pro", KeyEnv: "DEEPSEEK_API_KEY", Version: "2023-06-01"},
	{Name: "zai", BaseURL: "https://api.z.ai/api/anthropic", Model: "glm-5.3", KeyEnv: "ZAI_API_KEY", Bearer: true, Version: "2023-06-01"},
}

// NewMessages builds the brain for a provider. key must not be empty.
func NewMessages(p Provider, model, key string) *Messages {
	if model == "" {
		model = p.Model
	}
	return &Messages{
		BaseURL: p.BaseURL, Model: model, APIKey: key, Bearer: p.Bearer, Version: p.Version, Native: p.Native,
		MaxTokens: 16000,
		// No overall timeout: a turn may stream for minutes. The context
		// ends it when the agent is aborted.
		HTTP: &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 2 * time.Minute, Proxy: http.ProxyFromEnvironment}},
	}
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	// Eager makes tool input stream as it is generated. The input is then
	// not validated by the server, so it is parsed strictly here.
	Eager bool `json:"eager_input_streaming,omitempty"`
}

type wireMessage struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

type wireRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    string        `json:"system,omitempty"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
	Stream    bool          `json:"stream"`
	// Fallbacks lets the server re-run a request that a safety classifier
	// declined on another model, inside the same call.
	Fallbacks string `json:"fallbacks,omitempty"`
}

const fallbackBeta = "server-side-fallback-2026-07-01"

// encode turns a block of ours into the provider's JSON.
func encode(b Block) (json.RawMessage, error) {
	switch b.Type {
	case BlockText:
		return json.Marshal(map[string]any{"type": "text", "text": b.Text})
	case BlockToolUse:
		in := b.Input
		if len(in) == 0 {
			in = json.RawMessage("{}")
		}
		return json.Marshal(map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": in})
	case BlockToolResult:
		out := map[string]any{"type": "tool_result", "tool_use_id": b.ToolUseID, "content": b.Text}
		if b.IsError {
			out["is_error"] = true
		}
		return json.Marshal(out)
	}
	return nil, fmt.Errorf("llm: cannot send a block of type %q", b.Type)
}

func (m *Messages) request(req Request) ([]byte, error) {
	w := wireRequest{Model: m.Model, MaxTokens: m.MaxTokens, System: req.System, Stream: true}
	if m.Native {
		w.Fallbacks = "default"
	}
	for _, t := range req.Tools {
		w.Tools = append(w.Tools, wireTool{Name: t.Name, Description: t.Description, InputSchema: t.Schema, Eager: m.Native})
	}
	for _, msg := range req.Messages {
		wm := wireMessage{Role: msg.Role}
		if msg.Role == "assistant" && len(msg.Raw) > 0 {
			wm.Content = msg.Raw // replayed exactly as the provider sent it
		} else {
			// All tool results of a turn go back in one user message,
			// before any text.
			var results, rest []json.RawMessage
			for _, b := range msg.Blocks {
				raw, err := encode(b)
				if err != nil {
					return nil, err
				}
				if b.Type == BlockToolResult {
					results = append(results, raw)
				} else {
					rest = append(rest, raw)
				}
			}
			wm.Content = append(results, rest...)
		}
		if len(wm.Content) == 0 {
			continue
		}
		w.Messages = append(w.Messages, wm)
	}
	return json.Marshal(w)
}

// apiError is the error body of the Messages API.
type apiError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// classify maps a failure to the errors the agent loop knows. A usage or
// rate limit is the external abort of the design: nothing is wrong with
// the task. A conversation that no longer fits is the worker's cue to
// give up and report context_overflow.
func classify(status int, typ, message string) error {
	low := strings.ToLower(message)
	switch {
	case status == http.StatusTooManyRequests || typ == "rate_limit_error" || typ == "billing_error" || status == http.StatusPaymentRequired:
		return fmt.Errorf("%w: %s", ErrQuota, message)
	case strings.Contains(low, "prompt is too long") || strings.Contains(low, "context window") ||
		strings.Contains(low, "context length") || strings.Contains(low, "maximum context") || typ == "request_too_large":
		return fmt.Errorf("%w: %s", ErrContextOverflow, message)
	}
	if typ == "" {
		typ = "error"
	}
	return fmt.Errorf("llm: %s (HTTP %d): %s", typ, status, message)
}

// Stream sends the conversation and assembles the streamed reply.
func (m *Messages) Stream(ctx context.Context, req Request, onText func(string)) (Message, error) {
	body, err := m.request(req)
	if err != nil {
		return Message{}, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.BaseURL, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Message{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "text/event-stream")
	hr.Header.Set("anthropic-version", m.Version)
	if m.Bearer {
		hr.Header.Set("Authorization", "Bearer "+m.APIKey)
	} else {
		hr.Header.Set("x-api-key", m.APIKey)
	}
	if m.Native {
		hr.Header.Set("anthropic-beta", fallbackBeta)
	}
	resp, err := m.HTTP.Do(hr)
	if err != nil {
		return Message{}, fmt.Errorf("llm: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e apiError
		if json.Unmarshal(data, &e) != nil || e.Error.Message == "" {
			e.Error.Message = strings.TrimSpace(string(data))
		}
		return Message{}, classify(resp.StatusCode, e.Error.Type, e.Error.Message)
	}
	return ParseStream(resp.Body, onText)
}

// streamBlock is one content block while it is being streamed.
type streamBlock struct {
	typ       string
	start     json.RawMessage // the block as content_block_start gave it
	text      strings.Builder
	json      strings.Builder // a tool call's input, in fragments
	id, name  string
	signature string
}

// ParseStream reads the server-sent events of one streamed reply and
// assembles the message. It is exported so that recorded responses can be
// replayed against it in tests.
func ParseStream(r io.Reader, onText func(string)) (Message, error) {
	var blocks []*streamBlock
	var stopReason string
	var tokens int64
	at := func(i int) *streamBlock {
		for len(blocks) <= i {
			blocks = append(blocks, &streamBlock{})
		}
		return blocks[i]
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	done := false
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue // event names, comments, blank separators
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Usage struct {
					InputTokens  int64 `json:"input_tokens"`
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock json.RawMessage `json:"content_block"`
			Delta        struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return Message{}, fmt.Errorf("llm: malformed stream event: %w", err)
		}
		switch ev.Type {
		case "message_start":
			tokens += ev.Message.Usage.InputTokens
		case "content_block_start":
			b := at(ev.Index)
			b.start = ev.ContentBlock
			var head struct {
				Type, ID, Name, Text string
			}
			_ = json.Unmarshal(ev.ContentBlock, &head)
			b.typ, b.id, b.name = head.Type, head.ID, head.Name
			b.text.WriteString(head.Text)
		case "content_block_delta":
			b := at(ev.Index)
			switch ev.Delta.Type {
			case "text_delta":
				b.text.WriteString(ev.Delta.Text)
				if onText != nil {
					onText(ev.Delta.Text)
				}
			case "input_json_delta":
				b.json.WriteString(ev.Delta.PartialJSON)
			case "thinking_delta":
				b.text.WriteString(ev.Delta.Thinking)
			case "signature_delta":
				b.signature += ev.Delta.Signature
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
			tokens += ev.Usage.OutputTokens
		case "message_stop":
			done = true
		case "error":
			// An error can arrive mid-stream, after a 200.
			status := http.StatusInternalServerError
			if ev.Error.Type == "overloaded_error" {
				status = 529
			}
			return Message{}, classify(status, ev.Error.Type, ev.Error.Message)
		}
	}
	if err := sc.Err(); err != nil {
		return Message{}, fmt.Errorf("llm: the stream broke: %w", err)
	}
	if !done && stopReason == "" {
		return Message{}, fmt.Errorf("llm: the stream ended before the message was complete")
	}
	if stopReason == "refusal" {
		return Message{}, ErrRefused
	}

	msg := Message{Role: "assistant", Tokens: tokens}
	for _, b := range blocks {
		switch b.typ {
		case "text":
			if b.text.Len() == 0 {
				continue
			}
			raw, _ := json.Marshal(map[string]any{"type": "text", "text": b.text.String()})
			msg.Raw = append(msg.Raw, raw)
			msg.Blocks = append(msg.Blocks, Block{Type: BlockText, Text: b.text.String()})
		case "tool_use":
			input := strings.TrimSpace(b.json.String())
			if input == "" {
				input = "{}"
			}
			// The fragments must add up to one JSON object. If the reply
			// was cut off they do not; running a tool on a guess at the
			// rest would be worse than saying so.
			if !json.Valid([]byte(input)) || !strings.HasPrefix(input, "{") {
				if stopReason == "max_tokens" {
					return Message{}, fmt.Errorf("llm: the reply was cut off at max_tokens in the middle of a %s call", b.name)
				}
				return Message{}, fmt.Errorf("llm: the %s call has malformed input", b.name)
			}
			raw, _ := json.Marshal(map[string]any{"type": "tool_use", "id": b.id, "name": b.name, "input": json.RawMessage(input)})
			msg.Raw = append(msg.Raw, raw)
			msg.Blocks = append(msg.Blocks, Block{Type: BlockToolUse, ID: b.id, Name: b.name, Input: json.RawMessage(input)})
		case "thinking":
			// Kept only to be sent back unchanged; never shown or edited.
			raw, _ := json.Marshal(map[string]any{"type": "thinking", "thinking": b.text.String(), "signature": b.signature})
			msg.Raw = append(msg.Raw, raw)
		case "":
		default:
			// A block type this client does not know: replay it as it came.
			if len(b.start) > 0 {
				msg.Raw = append(msg.Raw, b.start)
			}
		}
	}
	return msg, nil
}
