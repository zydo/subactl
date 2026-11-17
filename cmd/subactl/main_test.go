package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/client"
	"github.com/zydo/subactl/internal/backend"
	"github.com/zydo/subactl/internal/daemon"
)

// daemonOnSocket serves a real subactld (fake backend) on a Unix socket.
// Identity comes from the socket's peer credentials, as in production, so
// the test process is an operator.
func daemonOnSocket(t *testing.T, setup ...func(*backend.Fake)) (socket string, f *backend.Fake) {
	t.Helper()
	// t.TempDir is too long for a Unix socket path on macOS.
	dir, err := os.MkdirTemp("/tmp", "subactl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket = filepath.Join(dir, "s")
	f = backend.NewFake()
	for _, fn := range setup { // before the daemon's goroutines start
		fn(f)
	}
	m, err := daemon.NewManager(context.Background(), daemon.Config{Backend: f, Capacity: 32 << 30, TermWait: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := daemon.NewServer(m, nil)
	srv := &http.Server{Handler: server, ConnContext: server.ConnContext}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return socket, f
}

type harness struct {
	t      *testing.T
	socket string
	uid    []string
}

// cli runs one subactl command and returns its exit code and output.
func (h harness) cli(args ...string) (code int, stdout, stderr string) {
	h.t.Helper()
	var out, errb bytes.Buffer
	code = run(append([]string{"--socket", h.socket}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

// ok runs a command that must succeed and decodes its JSON output into v.
func (h harness) ok(v any, args ...string) {
	h.t.Helper()
	code, out, errs := h.cli(args...)
	if code != 0 {
		h.t.Fatalf("subactl %s: exit %d\n%s%s", strings.Join(args, " "), code, out, errs)
	}
	if v != nil {
		if err := json.Unmarshal([]byte(out), v); err != nil {
			h.t.Fatalf("subactl %s: bad JSON: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

func TestEndToEnd(t *testing.T) {
	socket, f := daemonOnSocket(t)
	h := harness{t: t, socket: socket}
	if os.Getuid() == 0 { // root must name the uid an agent runs as
		h.uid = []string{"--uid", "1000"}
	}
	lead := func(args ...string) []string { return append(append([]string{"run"}, h.uid...), args...) }

	var host api.Host
	h.ok(&host, "host")
	if host.Backend != "fake" || host.EffectiveCapacity != 32<<30 || host.Capabilities.Memory != api.LevelHard {
		t.Fatalf("host %+v", host)
	}

	// A dry run checks admission and leaves nothing behind.
	var dry api.Lease
	h.ok(&dry, lead("--dry-run", "--memory", "4G")...)
	var leases []api.Lease
	h.ok(&leases, "ls")
	if dry.Envelope.Memory != 4<<30 || len(leases) != 0 {
		t.Fatalf("dry run: %+v, %d leases", dry, len(leases))
	}

	var ld api.Lease
	h.ok(&ld, lead("--wait=false", "--memory", "8G", "--memory-self", "1G", "--kind", "lead", "--", "agent", "--flag")...)
	if ld.OwnerUID != os.Getuid() || ld.Parent != "" || ld.Lead != ld.ID {
		t.Fatalf("lead %+v", ld)
	}
	id := string(ld.ID)

	var kid api.Lease
	h.ok(&kid, "spawn", "--parent", id, "--memory", "2G", "--kind", "worker", "--wall", "30m", "--", "worker")
	if kid.Parent != ld.ID || kid.Depth != 1 || kid.Envelope.Wall != 1800 {
		t.Fatalf("child %+v", kid)
	}

	var avail api.Available
	h.ok(&avail, "available", id)
	if avail.MemoryFree != 5<<30 || avail.LiveChildren != 1 {
		t.Fatalf("available %+v", avail)
	}

	// A refusal exits 1 with a structured reason on stderr.
	code, out, errs := h.cli("spawn", "--parent", id, "--memory", "6G", "--", "worker")
	_ = out
	var refusal api.Error
	if json.Unmarshal([]byte(errs), &refusal); code != 1 || refusal.Code != api.ErrRejected || refusal.Reason != api.ReasonParentBudget {
		t.Fatalf("exit %d, stderr %s", code, errs)
	}
	// A node cannot use "self" from outside a lease, and says how to fix it.
	code, _, errs = h.cli("available")
	if json.Unmarshal([]byte(errs), &refusal); code != 1 || refusal.Field != "id" || refusal.Example == "" {
		t.Fatalf("exit %d, stderr %s", code, errs)
	}

	var resized api.Lease
	h.ok(&resized, "resize", string(kid.ID), "--memory", "3G", "--pids", "128")
	if resized.Envelope.Memory != 3<<30 || resized.Envelope.Wall != 1800 || f.Limits(kid.ID).Pids != 128 {
		t.Fatalf("resize %+v", resized.Envelope)
	}

	h.ok(&leases, "ls", "--lead", id)
	if len(leases) != 2 || leases[0].ID != ld.ID || leases[1].Usage == nil {
		t.Fatalf("ls %+v", leases)
	}

	var term api.Termination
	h.ok(&term, "terminate", string(kid.ID), "--grace", "0s", "--code", "204")
	if term.Code != api.CodeReleased || term.Evidence["step"] != "sigterm" {
		t.Fatalf("terminate %+v", term)
	}
	var got api.Lease
	h.ok(&got, "get", string(kid.ID))
	if got.State != api.StateReaped || got.Termination == nil {
		t.Fatalf("get %+v", got)
	}

	// Observability through the CLI: suggestion, trace, metrics.
	code, out, errs = h.cli("suggest", "worker", "--lease", id)
	if code != 0 || strings.TrimSpace(out) != "2147483648" || !strings.Contains(errs, "default") {
		t.Fatalf("suggest: exit %d, %q, %q", code, out, errs)
	}
	var tr api.Trace
	h.ok(&tr, "trace", string(kid.ID))
	if tr.Termination == nil || tr.Termination.Code != api.CodeReleased || len(tr.Events) < 3 {
		t.Fatalf("trace %+v", tr)
	}
	h.ok(&tr, "audit", "--lease", string(kid.ID))
	code, out, _ = h.cli("metrics")
	if code != 0 || !strings.Contains(out, `subactl_terminations_total{code="204"} 1`) || !strings.Contains(out, `subactl_admission_rejections_total{reason="parent_budget"} 1`) {
		t.Fatalf("metrics: exit %d\n%s", code, out)
	}

	code, out, _ = h.cli("top", "--once")
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "capacity 32GiB") {
		t.Fatalf("top: exit %d\n%s", code, out)
	}

	h.ok(&term, "kill", id)
	if term.Code != api.CodeKilled {
		t.Fatalf("kill %+v", term)
	}
	var waited api.WaitChildrenResponse
	code, out, _ = h.cli("wait-children", id, "--self")
	if json.Unmarshal([]byte(out), &waited); code != 3 || !waited.Done || len(waited.Results) != 2 {
		t.Fatalf("wait-children exit %d: %s", code, out)
	}
	if refs, _ := f.List(context.Background()); len(refs) != 0 {
		t.Fatalf("acgroups left: %v", refs)
	}
}

// run blocks until the lead ends and exits by its termination code.
func TestRunWaits(t *testing.T) {
	socket, f := daemonOnSocket(t)
	h := harness{t: t, socket: socket}
	args := []string{"run", "--memory", "1G"}
	if os.Getuid() == 0 {
		args = append(args, "--uid", "1000")
	}
	for _, tc := range []struct {
		status   backend.ExitStatus
		wantExit int
		wantCode int
	}{{backend.ExitStatus{}, 0, api.CodeCompleted}, {backend.ExitStatus{Signal: 9}, 3, api.CodeExternalAbort}} {
		go func() {
			c := client.New(socket)
			for range 200 {
				leases, _ := c.Leases(context.Background(), "")
				for _, ls := range leases {
					if ls.State == api.StateRunning && f.ExitIfAlive(ls.ID, tc.status) {
						return
					}
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
		code, out, errs := h.cli(append(args, "--", "agent")...)
		var term api.Termination
		// The record of a lead goes to stderr; stdout is the lead's own output.
		if json.Unmarshal([]byte(errs), &term); code != tc.wantExit || term.Code != tc.wantCode {
			t.Fatalf("exit %d, want %d: %s%s", code, tc.wantExit, out, errs)
		}
	}
}

func TestCLIUsageErrors(t *testing.T) {
	h := harness{t: t, socket: "/nonexistent/subactl.sock"}
	for _, args := range [][]string{
		{}, {"frobnicate"}, {"get"}, {"run", "--memory", "lots", "--", "x"}, {"run", "--memory", "1G"},
		{"spawn", "-it", "--", "x"}, {"top", "--bogus"}, {"suggest"}, {"audit"}, {"trace"},
	} {
		if code, _, errs := h.cli(args...); code != 2 || errs == "" {
			t.Errorf("subactl %v: exit %d, stderr %q; want exit 2 with a message", args, code, errs)
		}
	}
	// A daemon that is not there is a plain failure, not a usage error.
	if code, _, errs := h.cli("host"); code == 0 || !strings.Contains(errs, "subactld") {
		t.Errorf("host without a daemon: exit %d, stderr %q", code, errs)
	}
}

// The HTTP layer refuses whole requests it does not understand.
func TestStrictHTTP(t *testing.T) {
	socket, f := daemonOnSocket(t)
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", socket)
	}}}
	do := func(method, path, body string) (int, api.Error) {
		t.Helper()
		req, _ := http.NewRequest(method, "http://subactld"+path, strings.NewReader(body))
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var e api.Error
		json.NewDecoder(resp.Body).Decode(&e)
		return resp.StatusCode, e
	}
	uid := ""
	if os.Getuid() == 0 {
		uid = `,"uid":1000`
	}
	for _, tc := range []struct {
		method, path, body string
		status             int
		code, field        string
	}{
		{"POST", "/v1/leases", `{"envelope":{"memory":1024,"memroy_self":1}` + uid + `}`, 400, api.ErrInvalidArgument, "memroy_self"},
		{"POST", "/v1/leases", `{"envelope":{"memory":"8G"}` + uid + `}`, 400, api.ErrInvalidArgument, "envelope.memory"},
		{"POST", "/v1/leases", ``, 400, api.ErrInvalidArgument, ""},
		{"POST", "/v1/leases", `{"envelope":{"memory":1024}` + uid + `} trailing`, 400, api.ErrInvalidArgument, ""},
		{"GET", "/v1/leases/ls_nope", ``, 404, api.ErrNotFound, ""},
		{"GET", "/v1/leases/ls_nope/attach", ``, 400, api.ErrInvalidArgument, "Upgrade"},
		{"POST", "/v1/leases/ls_nope/notify", `{"type":"NEAR_LIMIT"}`, 404, api.ErrNotFound, ""},
	} {
		status, e := do(tc.method, tc.path, tc.body)
		if status != tc.status || e.Code != tc.code || e.Field != tc.field || e.Message == "" {
			t.Errorf("%s %s %s: got %d %+v", tc.method, tc.path, tc.body, status, e)
		}
	}
	if refs, _ := f.List(context.Background()); len(refs) != 0 {
		t.Fatal(fmt.Sprint("refused requests had side effects: ", refs))
	}
}

// F25, the relay half: "run -it" shows what the lead writes to its
// terminal and delivers what the user types, until the lead ends.
func TestRunInteractive(t *testing.T) {
	socket, f := daemonOnSocket(t)
	typed, userEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin = typed
	defer func() { stdin = os.Stdin }()

	got := make(chan string, 1)
	go func() {
		// The "agent": find the lead, open its terminal, greet, read a line.
		c := client.New(socket)
		for range 400 {
			leases, _ := c.Leases(context.Background(), "")
			if len(leases) == 1 && leases[0].State == api.StateRunning {
				slave, err := os.OpenFile(f.Slave(leases[0].ID), os.O_RDWR, 0)
				if err != nil {
					got <- "open slave: " + err.Error()
					return
				}
				fmt.Fprint(slave, "ready> ")
				fmt.Fprintln(userEnd, "hello agent") // the user types a line
				buf := make([]byte, 256)
				n, _ := slave.Read(buf)
				fmt.Fprintf(slave, "you said: %s", buf[:n])
				got <- strings.TrimSpace(string(buf[:n]))
				// Let the output be relayed before the terminal goes away:
				// closing the last slave end may discard what is unread.
				time.Sleep(200 * time.Millisecond)
				f.Exit(leases[0].ID, backend.ExitStatus{})
				slave.Close()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		got <- "the lead never started"
	}()
	args := []string{"--socket", socket, "run", "-it", "--memory", "1G"}
	if os.Getuid() == 0 {
		args = append(args, "--uid", "1000")
	}
	var out, errs bytes.Buffer
	code := run(append(args, "--", "agent"), &out, &errs)
	if line := <-got; line != "hello agent" {
		t.Fatalf("the agent read %q from its terminal", line)
	}
	if code != 0 || !strings.Contains(out.String(), "ready> ") || !strings.Contains(out.String(), "you said: hello agent") {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out.String(), errs.String())
	}
	if !strings.Contains(errs.String(), "200 COMPLETED") {
		t.Fatalf("stderr %q", errs.String())
	}
}

// Every command the CLI has is offered by every completion script, and
// the scripts are valid in their shells.
func TestCompletion(t *testing.T) {
	h := harness{t: t, socket: "/nonexistent"}
	for _, shell := range []string{"bash", "zsh", "fish"} {
		code, out, errs := h.cli("completion", shell)
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", shell, code, errs)
		}
		for _, name := range commands {
			if !strings.Contains(out, name) {
				t.Errorf("%s completion lacks %q", shell, name)
			}
		}
		if path, err := exec.LookPath(shell); err == nil {
			script := filepath.Join(t.TempDir(), "completion")
			os.WriteFile(script, []byte(out), 0o644)
			if msg, err := exec.Command(path, "-n", script).CombinedOutput(); err != nil {
				t.Errorf("%s rejects its completion script: %v\n%s", shell, err, msg)
			}
		}
	}
	if code, _, _ := h.cli("completion", "tcsh"); code != 2 {
		t.Errorf("an unknown shell: exit %d", code)
	}
}

// The usage text and the completion list name the same commands.
func TestUsageListsEveryCommand(t *testing.T) {
	for _, name := range commands {
		if !strings.Contains(usage, "\n  "+name) {
			t.Errorf("the usage text does not list %q", name)
		}
	}
}

// F26: subactl is a plain executable. Started from bash, zsh or fish it
// behaves the same, with no shell function, alias or sourcing involved.
func TestShellAgnostic(t *testing.T) {
	socket, _ := daemonOnSocket(t)
	bin := filepath.Join(t.TempDir(), "subactl")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	uid := ""
	if os.Getuid() == 0 {
		uid = "--uid 1000 "
	}
	// A refused request and a successful one: stdout, stderr and status.
	script := fmt.Sprintf(`%s --socket %s run %s--dry-run --memory 100G -- agent; echo "status=$?"; %s --socket %s run %s--dry-run --memory 1G -- agent | grep -c '"memory": 1073741824'`,
		bin, socket, uid, bin, socket, uid)
	var first string
	ran := 0
	for _, shell := range []string{"bash", "zsh", "fish"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			if slices.Contains(strings.Fields(os.Getenv("SUBACTL_REQUIRE_SHELLS")), shell) {
				t.Fatalf("%s is required here but not installed", shell)
			}
			t.Logf("%s is not installed here; skipped", shell)
			continue
		}
		s := script
		if shell == "fish" {
			s = strings.ReplaceAll(s, "$?", "$status")
		}
		cmd := exec.Command(path, "-c", s)
		// A SHELL the tool must not care about.
		cmd.Env = append(os.Environ(), "SHELL=/bin/false")
		out, _ := cmd.CombinedOutput()
		got := string(out)
		if !strings.Contains(got, `"reason": "mem_budget"`) || !strings.Contains(got, "status=1") || !strings.HasSuffix(strings.TrimSpace(got), "1") {
			t.Fatalf("%s:\n%s", shell, got)
		}
		if ran++; first == "" {
			first = got
		} else if got != first {
			t.Errorf("%s behaves differently:\n%s\nwant:\n%s", shell, got, first)
		}
	}
	if ran == 0 {
		t.Skip("no shell to test with")
	}
}

// A lead started without a terminal still shows its output, as it is
// written, on the stdout of "subactl run".
func TestRunShowsLeadOutput(t *testing.T) {
	scratch := t.TempDir()
	socket, f := daemonOnSocket(t, func(f *backend.Fake) { f.ScratchBase = scratch })
	go func() {
		c := client.New(socket)
		for range 400 {
			leases, _ := c.Leases(context.Background(), "")
			if len(leases) == 1 && leases[0].State == api.StateRunning {
				dir := f.ScratchDir(leases[0].ID)
				os.WriteFile(filepath.Join(dir, "stdout"), []byte("working on it\n"), 0o644)
				os.WriteFile(filepath.Join(dir, "stderr"), []byte("a warning\n"), 0o644)
				time.Sleep(300 * time.Millisecond) // a few rounds of the follower
				f.ExitIfAlive(leases[0].ID, backend.ExitStatus{})
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	args := []string{"run", "--memory", "1G"}
	if os.Getuid() == 0 {
		args = append(args, "--uid", "1000")
	}
	code, out, errs := harness{t: t, socket: socket}.cli(append(args, "--", "agent")...)
	if code != 0 || out != "working on it\n" || !strings.HasPrefix(errs, "a warning\n") || !strings.Contains(errs, `"name": "COMPLETED"`) {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out, errs)
	}
}

// When the daemon goes away while "subactl run" waits, the message says
// what happened and that nothing was left running.
func TestRunDaemonGoesAway(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "subactl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "s")
	m, err := daemon.NewManager(context.Background(), daemon.Config{Backend: backend.NewFake(), Capacity: 32 << 30})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := daemon.NewServer(m, nil)
	srv := &http.Server{Handler: server, ConnContext: server.ConnContext}
	go srv.Serve(ln)
	go func() {
		c := client.New(socket)
		for range 400 {
			if leases, _ := c.Leases(context.Background(), ""); len(leases) == 1 && leases[0].State == api.StateRunning {
				srv.Close() // the daemon dies with the request open
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	args := []string{"--socket", socket, "run", "--memory", "1G"}
	if os.Getuid() == 0 {
		args = append(args, "--uid", "1000")
	}
	var out, errs bytes.Buffer
	code := run(append(args, "--", "agent"), &out, &errs)
	if code != 1 || !strings.Contains(errs.String(), "lost the connection to subactld") || !strings.Contains(errs.String(), "nothing is left running") {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
}
