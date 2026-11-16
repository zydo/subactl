package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func setup(t *testing.T) (map[string]Tool, string) {
	t.Helper()
	dir := t.TempDir()
	byName := map[string]Tool{}
	for _, tool := range All(Config{Dir: dir, SpillDir: filepath.Join(dir, "spill"), MaxOutput: 200, BashTimeout: 5 * time.Second}) {
		byName[tool.Spec().Name] = tool
	}
	return byName, dir
}

func call(t *testing.T, tool Tool, args map[string]any) (string, error) {
	t.Helper()
	in, _ := json.Marshal(args)
	return tool.Run(context.Background(), in)
}

func TestReadWriteEdit(t *testing.T) {
	tools, dir := setup(t)
	if _, err := call(t, tools["write"], map[string]any{"path": "a/b.txt", "content": "one\ntwo\nthree\ntwo\n"}); err != nil {
		t.Fatal(err)
	}
	if got, err := call(t, tools["read"], map[string]any{"path": "a/b.txt", "offset": 2, "limit": 2}); err != nil || got != "two\nthree\n" {
		t.Fatalf("read: %q, %v", got, err)
	}
	// edit must match exactly once, and says how to recover otherwise.
	_, err := call(t, tools["edit"], map[string]any{"path": "a/b.txt", "old": "two", "new": "2"})
	if err == nil || !strings.Contains(err.Error(), "matches 2 places") {
		t.Fatalf("ambiguous edit: %v", err)
	}
	_, err = call(t, tools["edit"], map[string]any{"path": "a/b.txt", "old": "four", "new": "4"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing edit: %v", err)
	}
	if _, err := call(t, tools["edit"], map[string]any{"path": "a/b.txt", "old": "two\nthree", "new": "2\n3"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "a/b.txt"))
	if string(data) != "one\n2\n3\ntwo\n" {
		t.Fatalf("file is %q", data)
	}
	if _, err := call(t, tools["read"], map[string]any{"path": "nope"}); err == nil {
		t.Fatal("read of a missing file succeeded")
	}
}

func TestBash(t *testing.T) {
	tools, dir := setup(t)
	bash := tools["bash"]
	if got, err := call(t, bash, map[string]any{"command": "echo hi; pwd"}); err != nil || !strings.HasPrefix(got, "hi\n") || !strings.Contains(got, filepath.Base(dir)) {
		t.Fatalf("got %q, %v", got, err)
	}
	// A failing command is an error that carries its output and status.
	_, err := call(t, bash, map[string]any{"command": "echo oops >&2; exit 3"})
	if err == nil || !strings.Contains(err.Error(), "oops") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("got %v", err)
	}
	// Long output is clipped and the full text is kept in a file.
	got, err := call(t, bash, map[string]any{"command": "seq 1 5000"})
	if err != nil || len(got) > 600 || !strings.Contains(got, "bytes omitted; the full output is in") || !strings.HasSuffix(got, "5000\n") {
		t.Fatalf("got %d bytes: %q, %v", len(got), got, err)
	}
	// A timeout kills the command's process group.
	start := time.Now()
	_, err = call(t, bash, map[string]any{"command": "sleep 30 & sleep 30", "timeout": 0.3})
	if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 5*time.Second {
		t.Fatalf("got %v after %v", err, time.Since(start))
	}
	// A background process left behind does not block the call.
	start = time.Now()
	if _, err := call(t, bash, map[string]any{"command": "sleep 20 >/dev/null 2>&1 & echo started"}); err != nil || time.Since(start) > 3*time.Second {
		t.Fatalf("blocked for %v: %v", time.Since(start), err)
	}
}

// With a mirror, a command's output is also copied out while it runs, and
// a background process left behind still does not block the call.
func TestBashMirror(t *testing.T) {
	dir := t.TempDir()
	var mirror lockedBuffer
	var bash Tool
	for _, tool := range All(Config{Dir: dir, SpillDir: filepath.Join(dir, "spill"), MaxOutput: 100, Mirror: &mirror}) {
		if tool.Spec().Name == "bash" {
			bash = tool
		}
	}
	start := time.Now()
	out, err := call(t, bash, map[string]any{"command": "seq 1 3000; sleep 20 >/dev/null 2>&1 &"})
	if err != nil || time.Since(start) > 3*time.Second {
		t.Fatalf("after %v: %v", time.Since(start), err)
	}
	// The model sees a clipped result; the mirror has all of it.
	if len(out) > 400 || !strings.HasSuffix(mirror.String(), "2999\n3000\n") || !strings.HasPrefix(mirror.String(), "1\n2\n") {
		t.Fatalf("result of %d bytes, mirror of %d bytes", len(out), len(mirror.String()))
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
