// Package tools holds the four tools mini-agent gives its model: read,
// write, edit and bash. Spawning a child agent is not a tool; the model
// does it from bash.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zydo/subactl/examples/mini-agent/llm"
)

// Tool is something the model can call.
type Tool interface {
	Spec() llm.Tool
	// Run returns the text shown to the model. An error is shown to the
	// model too, as a failed tool result, so it should say how to recover.
	Run(ctx context.Context, input json.RawMessage) (string, error)
}

// Config is shared by the four tools.
type Config struct {
	// Dir is the working directory for relative paths and for bash.
	Dir string
	// SpillDir receives the full output of every bash command.
	SpillDir string
	// MaxOutput caps what a tool returns to the model, in bytes.
	MaxOutput int
	// BashTimeout applies when a call names none.
	BashTimeout time.Duration
	// Env is the environment of bash commands; nil means the agent's own.
	Env []string
}

// All returns the four tools.
func All(c Config) []Tool {
	if c.MaxOutput == 0 {
		c.MaxOutput = 16 << 10
	}
	if c.BashTimeout == 0 {
		c.BashTimeout = 2 * time.Minute
	}
	return []Tool{&read{c}, &write{c}, &edit{c}, &bash{Config: c, shell: findShell()}}
}

func schema(s string) json.RawMessage { return json.RawMessage(s) }

func decode(input json.RawMessage, v any) error {
	if err := json.Unmarshal(input, v); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}

func (c Config) path(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

// clip keeps the head and tail of s when it exceeds max.
func clip(s string, max int, full string) string {
	if len(s) <= max {
		return s
	}
	half := max / 2
	note := fmt.Sprintf("\n[... %d bytes omitted", len(s)-2*half)
	if full != "" {
		note += "; the full output is in " + full
	}
	return s[:half] + note + " ...]\n" + s[len(s)-half:]
}

type read struct{ Config }

func (t *read) Spec() llm.Tool {
	return llm.Tool{Name: "read", Description: "Read a text file. offset and limit select lines (1-based).",
		Schema: schema(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["path"]}`)}
}

func (t *read) Run(_ context.Context, input json.RawMessage) (string, error) {
	var a struct {
		Path          string
		Offset, Limit int
	}
	if err := decode(input, &a); err != nil {
		return "", err
	}
	data, err := os.ReadFile(t.path(a.Path))
	if err != nil {
		return "", err
	}
	if a.Offset > 0 || a.Limit > 0 {
		lines := strings.SplitAfter(string(data), "\n")
		lo := max(a.Offset-1, 0)
		if lo > len(lines) {
			return "", fmt.Errorf("offset %d is past the end of the file (%d lines)", a.Offset, len(lines))
		}
		hi := len(lines)
		if a.Limit > 0 {
			hi = min(lo+a.Limit, hi)
		}
		data = []byte(strings.Join(lines[lo:hi], ""))
	}
	return clip(string(data), t.MaxOutput, t.path(a.Path)), nil
}

type write struct{ Config }

func (t *write) Spec() llm.Tool {
	return llm.Tool{Name: "write", Description: "Create or overwrite a file, creating parent directories.",
		Schema: schema(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`)}
}

func (t *write) Run(_ context.Context, input json.RawMessage) (string, error) {
	var a struct{ Path, Content string }
	if err := decode(input, &a); err != nil {
		return "", err
	}
	p := t.path(a.Path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(a.Content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.Path), nil
}

type edit struct{ Config }

func (t *edit) Spec() llm.Tool {
	return llm.Tool{Name: "edit", Description: "Replace one exact occurrence of old with new in a file. old must match exactly once.",
		Schema: schema(`{"type":"object","properties":{"path":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"}},"required":["path","old","new"]}`)}
}

func (t *edit) Run(_ context.Context, input json.RawMessage) (string, error) {
	var a struct{ Path, Old, New string }
	if err := decode(input, &a); err != nil {
		return "", err
	}
	if a.Old == "" {
		return "", errors.New("old is empty; to create a file use write")
	}
	p := t.path(a.Path)
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	switch n := strings.Count(string(data), a.Old); n {
	case 1:
	case 0:
		return "", fmt.Errorf("old was not found in %s; it must match exactly, including whitespace. Read the file again and copy the text", a.Path)
	default:
		return "", fmt.Errorf("old matches %d places in %s; include more surrounding lines so that it matches exactly once", n, a.Path)
	}
	info, _ := os.Stat(p)
	if err := os.WriteFile(p, []byte(strings.Replace(string(data), a.Old, a.New, 1)), info.Mode()); err != nil {
		return "", err
	}
	return "edited " + a.Path, nil
}

type bash struct {
	Config
	shell string
	seq   atomic.Int64
}

// findShell fixes the tool's shell to bash, whatever the user's $SHELL is:
// models write bash, and a fixed shell keeps experiments reproducible.
func findShell() string {
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "/bin/sh"
}

func (t *bash) Spec() llm.Tool {
	return llm.Tool{Name: "bash", Description: "Run a bash command. timeout is in seconds. Output beyond the cap is kept in a file whose path is returned.",
		Schema: schema(`{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"number"}},"required":["command"]}`)}
}

func (t *bash) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var a struct {
		Command string
		Timeout float64
	}
	if err := decode(input, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", errors.New("command is empty")
	}
	timeout := t.BashTimeout
	if a.Timeout > 0 {
		timeout = time.Duration(a.Timeout * float64(time.Second))
	}
	if err := os.MkdirAll(t.SpillDir, 0o755); err != nil {
		return "", err
	}
	// Output goes to a file, not a pipe: a pipe would keep the call blocked
	// for as long as any background process the command left behind lives.
	spill := filepath.Join(t.SpillDir, fmt.Sprintf("bash-%04d.out", t.seq.Add(1)))
	out, err := os.Create(spill)
	if err != nil {
		return "", err
	}
	defer out.Close()

	cmd := exec.Command(t.shell, "-c", a.Command)
	cmd.Dir, cmd.Env = t.Dir, t.Env
	cmd.Stdout, cmd.Stderr = out, out
	// The command gets its own process group so that a timeout can take its
	// foreground children with it, as mainstream harnesses do.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var note string
	var werr error
	select {
	case werr = <-done:
	case <-timer.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		werr = <-done
		note = fmt.Sprintf("\n[timed out after %v; the command's process group was killed]", timeout)
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return "", ctx.Err()
	}
	text := tail(out, spill, t.MaxOutput) + note
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			return "", fmt.Errorf("%s\n[exit status %d]", text, ee.ExitCode())
		}
		return "", fmt.Errorf("%s\n[%v]", text, werr)
	}
	if text == "" {
		text = "[no output]"
	}
	return text, nil
}

// tail returns the head and tail of the spill file without reading all of
// it: a runaway command may have written gigabytes.
func tail(f *os.File, path string, max int) string {
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	if size <= int64(max) {
		data, _ := os.ReadFile(path)
		return string(data)
	}
	half := int64(max / 2)
	head := make([]byte, half)
	end := make([]byte, half)
	_, _ = f.ReadAt(head, 0)
	if _, err := f.ReadAt(end, size-half); err != nil && err != io.EOF {
		return string(head)
	}
	return fmt.Sprintf("%s\n[... %d bytes omitted; the full output is in %s ...]\n%s", head, size-2*half, path, end)
}
