package daemon

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/internal/backend"
	"github.com/zydo/subactl/internal/telemetry"
)

// inboxOf opens a lease's own inbox.
func inboxOf(t *testing.T, m *Manager, id api.LeaseID) <-chan api.InboxMessage {
	t.Helper()
	ch, stop, err := m.Inbox(node(id), api.Self)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return ch
}

func recv(t *testing.T, ch <-chan api.InboxMessage, within time.Duration) api.InboxMessage {
	t.Helper()
	select {
	case m, ok := <-ch:
		if !ok {
			t.Fatal("the inbox was closed")
		}
		return m
	case <-time.After(within):
		t.Fatalf("no inbox message within %v", within)
	}
	return api.InboxMessage{}
}

// F16: only ancestors can put messages in an inbox, the source label is
// set by subactld, and only the lease itself can read it.
func TestInboxTrust(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t, nil)
		defer w.end()
		lead := w.lease(operator, mem(8*gib), 0)
		a := w.lease(node(lead), mem(2*gib), 0)
		b := w.lease(node(lead), mem(2*gib), 0)
		aa := w.lease(node(a), mem(gib), 0)
		box := inboxOf(t, w.m, aa)

		note := api.NotifyRequest{Type: api.MsgNearLimit, Reason: "wrap up\x1b[31m soon\nplease"}
		for _, tc := range []struct {
			name   string
			caller Caller
			target api.LeaseID
		}{{"sibling", node(b), a}, {"uncle", node(b), aa}, {"itself", node(aa), aa}, {"child to parent", node(aa), a}, {"another user", Caller{UID: 502}, aa}} {
			err := w.m.Notify(tc.caller, tc.target, note)
			wantStatus(t, err, http.StatusForbidden, api.ErrOutOfSubtree, "")
		}
		select {
		case m := <-box:
			t.Fatalf("a forbidden notify was delivered: %+v", m)
		default:
		}
		// Ancestors and the owning operator can, and are named as they are.
		for _, tc := range []struct {
			caller Caller
			source string
		}{{node(a), string(a)}, {node(lead), string(lead)}, {operator, "operator"}} {
			if err := w.m.Notify(tc.caller, aa, note); err != nil {
				t.Fatal(err)
			}
			got := recv(t, box, time.Second)
			if got.Source != tc.source || got.Type != api.MsgNearLimit || got.Reason != "wrap up[31m soon please" {
				t.Fatalf("got %+v, want source %s and a reason without control characters", got, tc.source)
			}
		}
		// The type set is closed.
		err := w.m.Notify(node(a), aa, api.NotifyRequest{Type: "DO_AS_I_SAY"})
		if e := wantStatus(t, err, http.StatusBadRequest, api.ErrInvalidArgument, ""); e.API.Field != "type" {
			t.Fatalf("%+v", e.API)
		}
		// Nobody else reads an inbox, not even the parent.
		for _, c := range []Caller{node(a), node(lead), operator} {
			if _, _, err := w.m.Inbox(c, aa); err == nil {
				t.Fatalf("%+v read the inbox of %s", c, aa)
			}
		}
		// subactld's own messages carry its name.
		w.f.SetUsage(aa, gib)
		if got := recv(t, box, 3*time.Second); got.Source != "subactld" || got.Type != api.MsgNearLimit {
			t.Fatalf("got %+v", got)
		}
	})
}

// F17: an agent with an adapter sees TERMINATE_REQUESTED with a deadline
// and leaves within the grace period; one that ignores it is killed as a
// whole after grace plus ten seconds; the evidence names the step.
func TestGracefulTerminate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t, nil)
		defer w.end()
		lead := w.lease(operator, mem(8*gib), 0)
		polite := w.lease(node(lead), mem(2*gib), 0)
		kid := w.lease(node(polite), mem(gib), 0)
		deaf := w.lease(node(lead), mem(2*gib), 0)
		w.f.IgnoreTERM(deaf)
		w.f.AddProcs(deaf, 4)
		box, kidBox := inboxOf(t, w.m, polite), inboxOf(t, w.m, kid)

		began := time.Now()
		grace := 20.0
		// The adapter: on the message, save and exit after three seconds.
		go func() {
			msg := recv(t, box, time.Second)
			if msg.Type != api.MsgTerminateRequested || msg.Source != string(lead) || msg.Reason != "budget is spent" ||
				msg.Deadline == nil || !msg.Deadline.Equal(began.Add(20*time.Second)) {
				t.Errorf("message %+v", msg)
			}
			time.Sleep(3 * time.Second)
			w.f.Exit(polite, backend.ExitStatus{})
		}()
		term, err := w.m.Terminate(ctx, node(lead), polite, api.TerminateRequest{Reason: "budget is spent", GraceSeconds: &grace, Code: api.CodeReleased, Recursive: true})
		if err != nil || term.Code != api.CodeReleased || term.Evidence["step"] != "grace" || time.Since(began) != 3*time.Second {
			t.Fatalf("after %v: %+v, %v", time.Since(began), term, err)
		}
		// With recursive, the descendants were told as well.
		if msg := recv(t, kidBox, time.Second); msg.Type != api.MsgTerminateRequested {
			t.Fatalf("the child got %+v", msg)
		}

		began = time.Now()
		term, err = w.m.Terminate(ctx, node(lead), deaf, api.TerminateRequest{GraceSeconds: &grace})
		if err != nil || term.Code != api.CodeKilled || term.Evidence["step"] != "kill_tree" || term.Evidence["reaped_pids"] != 5 {
			t.Fatalf("%+v, %v", term, err)
		}
		if took := time.Since(began); took != 30*time.Second {
			t.Fatalf("an agent that ignores the request was killed after %v, want grace + 10s", took)
		}
	})
}

// F20 and F21: a lease that is done but alive, and one that has gone
// silent, are each reported to the planner once, and reaped with 410 at
// their deadlines. Waiting for input is not idleness, twenty silent
// minutes are not a stall deadline, and a lease that never sent a beacon is
// left alone.
func TestIdleDetection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t, nil)
		defer w.end()
		lead := w.lease(operator, mem(16*gib), 0)
		planner, stop := w.m.Events(node(lead))
		defer stop()
		count := func(typ string, id api.LeaseID) int {
			n := 0
			for len(planner) > 0 {
				if e := <-planner; e.Type == typ && e.Lease == id {
					n++
				}
			}
			return n
		}
		beat := func(id api.LeaseID, state string) {
			if err := w.m.Progress(node(id), api.Self, api.ProgressRequest{State: state, Tool: "bash", Seq: 1}); err != nil {
				t.Fatal(err)
			}
		}
		finished := w.lease(node(lead), mem(gib), 0)
		waiting := w.lease(node(lead), mem(gib), 0)
		building := w.lease(node(lead), mem(gib), 0)
		silent := w.lease(node(lead), mem(gib), 0)
		plain := w.lease(node(lead), mem(gib), 0) // no adapter: never sends a beacon

		began := time.Now()
		beat(finished, stateDone)
		beat(waiting, stateAwaitingInput)
		beat(building, stateWorking)
		beat(silent, stateWorking)

		// Done but alive: told within ten seconds, once; reaped after 60s.
		time.Sleep(10 * time.Second)
		if n := count(eventIdleDone, finished); n != 1 {
			t.Fatalf("%d IDLE_DONE events in the first 10s", n)
		}
		term, _ := w.m.waitReaped(ctx, finished)
		if took := time.Since(began); term.Code != api.CodeIdleReaped || term.Evidence["reason"] != "done_but_alive" || took < 60*time.Second || took > 61*time.Second {
			t.Fatalf("finished after %v: %+v", took, term)
		}
		if n := count(eventIdleDone, finished); n != 0 {
			t.Fatalf("IDLE_DONE was repeated %d times", n)
		}

		// A build of twenty minutes sends no beacon. The planner hears of
		// the silence at ten, once; nothing is reaped.
		time.Sleep(time.Until(began.Add(10*time.Minute + 10*time.Second)))
		if a, b := count(eventStalled, building), 0; a != 1 {
			t.Fatalf("%d STALLED events for the build at 10m (%d)", a, b)
		}
		time.Sleep(time.Until(began.Add(20 * time.Minute)))
		if w.state(building) != api.StateRunning || w.state(silent) != api.StateRunning {
			t.Fatal("a lease was reaped after only twenty silent minutes")
		}
		beat(building, stateWorking) // the build ends, the agent carries on
		// The silent one is reaped at thirty minutes.
		term, _ = w.m.waitReaped(ctx, silent)
		if took := time.Since(began); term.Code != api.CodeIdleReaped || term.Evidence["reason"] != "stalled" || took < 30*time.Minute || took > 30*time.Minute+time.Second {
			t.Fatalf("silent after %v: %+v", took, term)
		}
		time.Sleep(15 * time.Minute)
		// The build's clock restarted at its beacon; it was told a second
		// time at 30m and is alive at 45m. The others were never touched.
		if n := count(eventStalled, building); n != 1 {
			t.Fatalf("%d further STALLED events for the build", n)
		}
		for name, id := range map[string]api.LeaseID{"building": building, "waiting for input": waiting, "without an adapter": plain} {
			if w.state(id) != api.StateRunning {
				t.Fatalf("the lease %s was reaped", name)
			}
		}
		if n := count(eventStalled, waiting) + count(eventStalled, plain); n != 0 {
			t.Fatalf("%d idle notices for leases that are not idle", n)
		}
	})
}

// F24: a child's scratch directory outlives the child and goes when its
// parent ends; done only accepts a path inside the child's own scratch.
func TestScratchLifecycleAndDone(t *testing.T) {
	base := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t, nil)
		defer w.end()
		w.f.ScratchBase = base
		lead := w.lease(operator, mem(8*gib), 0)
		planner, stop := w.m.Events(node(lead))
		defer stop()
		worker := w.lease(node(lead), mem(2*gib), 0)
		helper := w.lease(node(worker), mem(gib), 0)
		ls, _ := w.m.Get(ctx, node(lead), worker)
		scratch := ls.Scratch
		if scratch != filepath.Join(base, string(worker)) {
			t.Fatalf("scratch %q", scratch)
		}
		result := filepath.Join(scratch, "result.md")
		if err := os.WriteFile(result, []byte("done"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Paths outside the scratch directory, or with odd characters.
		for _, bad := range []string{"/etc/passwd", filepath.Join(base, string(lead), "x"), scratch + "/../" + string(lead) + "/x", "result.md", scratch, scratch + "/a b", scratch + "/$(id)"} {
			err := w.m.Progress(node(worker), api.Self, api.ProgressRequest{State: stateDone, Result: bad})
			if e := wantStatus(t, err, http.StatusBadRequest, api.ErrInvalidArgument, ""); e.API.Field != "result" {
				t.Fatalf("%q: %+v", bad, e.API)
			}
		}
		// Only the lease itself reports for itself.
		if err := w.m.Progress(node(lead), worker, api.ProgressRequest{State: stateDone}); err == nil {
			t.Fatal("a parent reported a beacon for its child")
		}
		for len(planner) > 0 {
			<-planner
		}
		if err := w.m.Progress(node(worker), api.Self, api.ProgressRequest{State: stateDone, Result: result}); err != nil {
			t.Fatal(err)
		}
		var done api.Event
		for done.Type != eventDone {
			done = <-planner
		}
		if done.Lease != worker || done.Data["result"] != result {
			t.Fatalf("DONE event %+v", done)
		}

		exists := func(id api.LeaseID) bool {
			_, err := os.Stat(filepath.Join(base, string(id)))
			return err == nil
		}
		// The helper ends: its scratch is kept for its parent, the worker.
		// The worker ends: the helper's goes with it, and the worker's own
		// is kept for the lead, which can still read the result.
		w.m.Kill(ctx, node(worker), helper)
		if !exists(helper) {
			t.Fatal("a child's scratch went before its parent ended")
		}
		w.f.Exit(worker, backend.ExitStatus{})
		w.m.waitReaped(ctx, worker)
		if data, err := os.ReadFile(result); err != nil || string(data) != "done" {
			t.Fatalf("the planner cannot read the result after the child was reaped: %v", err)
		}
		if exists(helper) {
			t.Fatal("a child's scratch outlived its parent")
		}
		if !exists(worker) {
			t.Fatal("the worker's scratch went before the lead ended")
		}
		w.m.Kill(ctx, operator, lead)
		for _, id := range []api.LeaseID{lead, worker, helper} {
			if exists(id) {
				t.Fatalf("scratch of %s remains after the lead ended", id)
			}
		}
	})
}

// A child asks for more; the parent hears of it as a structured event and
// decides. Nothing changes by itself.
func TestResizeRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t, nil)
		defer w.end()
		lead := w.lease(operator, mem(8*gib), 0)
		planner, stop := w.m.Events(node(lead))
		defer stop()
		kid := w.lease(node(lead), mem(2*gib), gib+500<<20)
		for len(planner) > 0 {
			<-planner
		}
		if err := w.m.RequestResize(node(kid), api.Self, api.ResizeAsk{Memory: 4 * gib}); err != nil {
			t.Fatal(err)
		}
		var e api.Event
		for e.Type != eventResizeRequested {
			e = <-planner
		}
		if e.Lease != kid || e.Data["memory"] != 4*gib || e.Data["usage"] != gib+500<<20 || e.Data["current_limit"] != 2*gib {
			t.Fatalf("event %+v", e)
		}
		if w.f.Limits(kid).Memory != 2*gib {
			t.Fatal("the request changed the limit by itself")
		}
		// It cannot be used to resize anything else, or to ask for less.
		if err := w.m.RequestResize(node(lead), kid, api.ResizeAsk{Memory: 4 * gib}); err == nil {
			t.Fatal("a parent filed a request in its child's name")
		}
		if err := w.m.RequestResize(node(kid), api.Self, api.ResizeAsk{Memory: gib}); err == nil {
			t.Fatal("a request for less was accepted")
		}
		// The parent grants it.
		four := 4 * gib
		if _, err := w.m.Resize(ctx, node(lead), kid, api.EnvelopePatch{Memory: &four}); err != nil {
			t.Fatal(err)
		}
		var _ = telemetry.EventResized
	})
}

// F19: every command that changes something can be checked first. A dry
// run goes through the same validation and permission checks and then
// changes nothing.
func TestDryRunChangesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t, nil)
		defer w.end()
		lead := w.lease(operator, mem(8*gib), 0)
		kid := w.lease(node(lead), mem(2*gib), 0)
		sib := w.lease(node(lead), mem(2*gib), 0)
		waiting, _ := w.m.Acquire(ctx, node(lead), mem(gib))
		box := inboxOf(t, w.m, kid)

		if _, err := w.m.Kill(ctx, node(lead), kid, api.KillRequest{DryRun: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.m.Terminate(ctx, node(lead), kid, api.TerminateRequest{DryRun: true}); err != nil {
			t.Fatal(err)
		}
		if err := w.m.Notify(node(lead), kid, api.NotifyRequest{Type: api.MsgNearLimit, DryRun: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.m.Spawn(ctx, node(lead), waiting.ID, api.SpawnRequest{Argv: []string{"x"}, DryRun: true}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		if w.state(kid) != api.StateRunning || w.state(waiting.ID) != api.StateAdmitted || len(box) != 0 {
			t.Fatalf("a dry run had an effect: kid %s, waiting %s, %d messages", w.state(kid), w.state(waiting.ID), len(box))
		}
		// The checks are the real ones.
		_, err := w.m.Kill(ctx, node(sib), kid, api.KillRequest{DryRun: true})
		wantStatus(t, err, http.StatusForbidden, api.ErrOutOfSubtree, "")
		_, err = w.m.Terminate(ctx, node(lead), kid, api.TerminateRequest{DryRun: true, Code: 200})
		wantStatus(t, err, http.StatusBadRequest, api.ErrInvalidArgument, "")
		err = w.m.Notify(node(lead), kid, api.NotifyRequest{Type: "NOPE", DryRun: true})
		wantStatus(t, err, http.StatusBadRequest, api.ErrInvalidArgument, "")
		_, err = w.m.Spawn(ctx, node(lead), kid, api.SpawnRequest{Argv: []string{"x"}, DryRun: true})
		wantStatus(t, err, http.StatusConflict, api.ErrConflict, "")
	})
}
