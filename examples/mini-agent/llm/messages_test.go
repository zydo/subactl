package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func open(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// A recorded reply with reasoning, text and a tool call whose input
// arrives in fragments.
func TestParseStreamToolUse(t *testing.T) {
	var said strings.Builder
	msg, err := ParseStream(open(t, "tool_use.sse"), func(s string) { said.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if said.String() != "I'll list the files." || msg.Tokens != 412+58 {
		t.Fatalf("text %q, tokens %d", said.String(), msg.Tokens)
	}
	if len(msg.Blocks) != 2 || msg.Blocks[0].Text != "I'll list the files." {
		t.Fatalf("blocks %+v", msg.Blocks)
	}
	call := msg.Blocks[1]
	var in struct {
		Command string
		Timeout float64
	}
	if err := json.Unmarshal(call.Input, &in); err != nil || call.ID != "toolu_01A" || call.Name != "bash" || in.Command != `ls -la "my dir"` || in.Timeout != 30 {
		t.Fatalf("call %+v, input %+v, %v", call, in, err)
	}
	// The raw form keeps the reasoning block, with its signature, in
	// place: it must go back unchanged on the next request.
	if len(msg.Raw) != 3 {
		t.Fatalf("%d raw blocks", len(msg.Raw))
	}
	var thinking struct{ Type, Signature string }
	if json.Unmarshal(msg.Raw[0], &thinking); thinking.Type != "thinking" || thinking.Signature != "EqQBCkYIAxgC" {
		t.Fatalf("first raw block %s", msg.Raw[0])
	}
}

func TestParseStreamFailures(t *testing.T) {
	// Cut off inside a tool call: the tool must not run on half an input.
	if _, err := ParseStream(open(t, "truncated_tool.sse"), nil); err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("truncated tool call: %v", err)
	}
	// An error event after the stream began.
	if _, err := ParseStream(open(t, "overloaded.sse"), nil); err == nil || !strings.Contains(err.Error(), "overloaded_error") {
		t.Fatalf("overloaded: %v", err)
	}
	if _, err := ParseStream(open(t, "refusal.sse"), nil); !errors.Is(err, ErrRefused) {
		t.Fatalf("refusal: %v", err)
	}
	// A connection that drops before the message is complete.
	data, _ := io.ReadAll(open(t, "tool_use.sse"))
	if _, err := ParseStream(strings.NewReader(string(data[:len(data)/2])), nil); err == nil {
		t.Fatal("half a stream was accepted as a message")
	}
	if _, err := ParseStream(strings.NewReader("data: {not json\n\n"), nil); err == nil {
		t.Fatal("a malformed event was accepted")
	}
}

// The request that goes out, and how the provider's failures are read.
func TestMessagesRequestAndErrors(t *testing.T) {
	var got struct {
		Path, Key, Auth, Version, Beta string
		Body                           map[string]any
	}
	reply := func(w http.ResponseWriter) {
		data, _ := os.ReadFile("testdata/tool_use.sse")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(data)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Path, got.Key, got.Auth = r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization")
		got.Version, got.Beta = r.Header.Get("anthropic-version"), r.Header.Get("anthropic-beta")
		got.Body = nil
		json.NewDecoder(r.Body).Decode(&got.Body)
		reply(w)
	}))
	defer srv.Close()

	first, err := ParseStream(open(t, "tool_use.sse"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req := Request{
		System: "You are mini-agent.",
		Tools:  []Tool{{Name: "bash", Description: "Run a command.", Schema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []Message{
			{Role: "user", Blocks: []Block{{Type: BlockText, Text: "list the files"}}},
			first,
			{Role: "user", Blocks: []Block{
				{Type: BlockText, Text: "a note from the planner"},
				{Type: BlockToolResult, ToolUseID: "toolu_01A", Text: "a.txt", IsError: true},
			}},
		},
	}
	native := NewMessages(Provider{BaseURL: srv.URL + "/", Model: "claude-opus-5-5", Version: "2026-10-21", Native: true}, "", "sk-test")
	if _, err := native.Stream(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if got.Path != "/v1/messages" || got.Key != "sk-test" || got.Auth != "" || got.Version != "2026-10-21" || got.Beta != fallbackBeta {
		t.Fatalf("request %+v", got)
	}
	b := got.Body
	if b["model"] != "claude-opus-5-5" || b["stream"] != true || b["fallbacks"] != "default" || b["system"] != "You are mini-agent." {
		t.Fatalf("body %v", b)
	}
	if tool := b["tools"].([]any)[0].(map[string]any); tool["eager_input_streaming"] != true || tool["name"] != "bash" {
		t.Fatalf("tool %v", tool)
	}
	msgs := b["messages"].([]any)
	// The assistant turn goes back as it came, reasoning block first.
	assistant := msgs[1].(map[string]any)["content"].([]any)
	if len(assistant) != 3 || assistant[0].(map[string]any)["signature"] != "EqQBCkYIAxgC" {
		t.Fatalf("assistant turn %v", assistant)
	}
	// Tool results come before text in the user turn that carries them.
	user := msgs[2].(map[string]any)["content"].([]any)
	if r := user[0].(map[string]any); r["type"] != "tool_result" || r["tool_use_id"] != "toolu_01A" || r["is_error"] != true {
		t.Fatalf("user turn %v", user)
	}

	// A compatible endpoint: bearer key, and none of the native-only fields.
	other := NewMessages(Provider{BaseURL: srv.URL, Model: "glm-5.3", Bearer: true, Version: "2023-06-01"}, "glm-5.3-flash", "zk-test")
	if _, err := other.Stream(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if got.Auth != "Bearer zk-test" || got.Key != "" || got.Beta != "" || got.Body["model"] != "glm-5.3-flash" {
		t.Fatalf("request %+v", got)
	}
	if _, has := got.Body["fallbacks"]; has {
		t.Fatal("a compatible endpoint was sent the fallbacks field")
	}
	if _, has := got.Body["tools"].([]any)[0].(map[string]any)["eager_input_streaming"]; has {
		t.Fatal("a compatible endpoint was sent eager_input_streaming")
	}

	// Failures.
	fail := func(status int, body string) error {
		reply = func(w http.ResponseWriter) {
			w.WriteHeader(status)
			io.WriteString(w, body)
		}
		_, err := native.Stream(context.Background(), req, nil)
		return err
	}
	if err := fail(429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`); !errors.Is(err, ErrQuota) {
		t.Fatalf("429: %v", err)
	}
	if err := fail(402, `{"error":{"type":"billing_error","message":"Insufficient Balance"}}`); !errors.Is(err, ErrQuota) {
		t.Fatalf("402: %v", err)
	}
	if err := fail(400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 1200000 tokens > 1000000 maximum"}}`); !errors.Is(err, ErrContextOverflow) {
		t.Fatalf("too long: %v", err)
	}
	if err := fail(401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`); err == nil || errors.Is(err, ErrQuota) || !strings.Contains(err.Error(), "authentication_error") {
		t.Fatalf("401: %v", err)
	}
	if err := fail(502, `<html>bad gateway</html>`); err == nil || !strings.Contains(err.Error(), "bad gateway") {
		t.Fatalf("502: %v", err)
	}
}
