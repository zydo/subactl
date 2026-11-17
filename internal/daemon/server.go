package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/user"
	"strconv"
	"strings"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/internal/backend"
)

// IdentityFunc decides who is calling. The daemon never issues tokens:
// identity comes from the connection, not from anything the caller says.
type IdentityFunc func(r *http.Request) (Caller, error)

// Route is one operation of the contract.
type Route struct {
	ID      string
	Method  string
	Path    string
	handler func(s *Server, w http.ResponseWriter, r *http.Request, c Caller) error
	// Pending names the milestone that implements the route. Until then it
	// answers 501.
	Pending string
}

// Server is the HTTP face of a Manager.
type Server struct {
	m        *Manager
	identify IdentityFunc
	mux      *http.ServeMux
}

// Routes is the route table. A conformance test holds it against
// api/openapi.yaml in both directions.
func Routes() []Route {
	return []Route{
		{ID: "getHost", Method: "GET", Path: "/v1/host", handler: (*Server).getHost},
		{ID: "createLease", Method: "POST", Path: "/v1/leases", handler: (*Server).createLease},
		{ID: "listLeases", Method: "GET", Path: "/v1/leases", handler: (*Server).listLeases},
		{ID: "getLease", Method: "GET", Path: "/v1/leases/{id}", handler: (*Server).getLease},
		{ID: "resizeLease", Method: "PATCH", Path: "/v1/leases/{id}", handler: (*Server).resizeLease},
		{ID: "getAvailable", Method: "GET", Path: "/v1/leases/{id}/available", handler: (*Server).getAvailable},
		{ID: "spawn", Method: "POST", Path: "/v1/leases/{id}/spawn", handler: (*Server).spawn},
		{ID: "terminate", Method: "POST", Path: "/v1/leases/{id}/terminate", handler: (*Server).terminate},
		{ID: "kill", Method: "POST", Path: "/v1/leases/{id}/kill", handler: (*Server).kill},
		{ID: "waitChildren", Method: "POST", Path: "/v1/leases/{id}/wait-children", handler: (*Server).waitChildren},
		{ID: "attach", Method: "GET", Path: "/v1/leases/{id}/attach", handler: (*Server).attach},
		{ID: "requestResize", Method: "POST", Path: "/v1/leases/{id}/resize-request", handler: (*Server).requestResize},
		{ID: "notify", Method: "POST", Path: "/v1/leases/{id}/notify", handler: (*Server).notify},
		{ID: "reportProgress", Method: "POST", Path: "/v1/leases/{id}/progress", handler: (*Server).reportProgress},
		{ID: "getTrace", Method: "GET", Path: "/v1/leases/{id}/trace", handler: (*Server).getTrace},
		{ID: "streamInbox", Method: "GET", Path: "/v1/leases/{id}/inbox", handler: (*Server).streamInbox},
		{ID: "streamEvents", Method: "GET", Path: "/v1/events", handler: (*Server).streamEvents},
		{ID: "getMetrics", Method: "GET", Path: "/metrics", handler: (*Server).getMetrics},
	}
}

// NewServer wires the routes. identify may be nil, in which case identity
// comes from the peer credentials of the Unix socket: set the server's
// ConnContext method as http.Server.ConnContext.
func NewServer(m *Manager, identify IdentityFunc) *Server {
	s := &Server{m: m, identify: identify, mux: http.NewServeMux()}
	if s.identify == nil {
		s.identify = s.peerIdentity
	}
	for _, rt := range Routes() {
		s.mux.HandleFunc(rt.Method+" "+rt.Path, func(w http.ResponseWriter, r *http.Request) {
			c, err := s.identify(r)
			if err != nil {
				writeErr(w, apiErr(http.StatusForbidden, api.ErrOutOfSubtree, "cannot establish the caller's identity: %v", err))
				return
			}
			if rt.handler == nil {
				writeErr(w, apiErr(http.StatusNotImplemented, api.ErrNotImplemented, "%s lands in milestone %s", rt.ID, rt.Pending))
				return
			}
			if err := rt.handler(s, w, r, c); err != nil {
				writeErr(w, err)
			}
		})
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

type peerKey struct{}

type peer struct {
	caller Caller
	err    error
}

// ConnContext establishes, once per connection, who is at the other end.
// Set it as http.Server.ConnContext.
//
// The peer's pid is mapped to the acgroup it is in. Between reading the
// pid and looking it up, the process could exit and its pid be given to
// another process in another acgroup. Where the kernel hands out a pidfd
// of the peer (Linux 6.5), that is ruled out: the lookup only counts if
// the original process is still alive after it.
func (s *Server) ConnContext(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return context.WithValue(ctx, peerKey{}, peer{err: errors.New("not a Unix socket connection")})
	}
	pid, uid, pidfd, err := peerCred(uc)
	if err != nil {
		return context.WithValue(ctx, peerKey{}, peer{err: err})
	}
	if pidfd >= 0 {
		defer closeFD(pidfd)
	}
	caller := Caller{UID: uid, PID: pid}
	ref, err := s.m.be.Identify(ctx, pid)
	switch {
	case pidfd >= 0 && exited(pidfd):
		err = errors.New("the calling process exited before it could be identified")
	case err == nil:
		// A process inside an acgroup is that node.
		caller.Lease = ref.Lease
	case errors.Is(err, backend.ErrNotMember):
		// Any other process is an operator. Root and members of the
		// subactl-admin group may act on every lead.
		caller.Admin, err = uid == 0 || inAdminGroup(uid), nil
	}
	return context.WithValue(ctx, peerKey{}, peer{caller, err})
}

// peerIdentity returns what ConnContext established for this connection.
func (s *Server) peerIdentity(r *http.Request) (Caller, error) {
	p, ok := r.Context().Value(peerKey{}).(peer)
	if !ok {
		return Caller{}, errors.New("no peer credentials on this connection")
	}
	return p.caller, p.err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		e = apiErr(http.StatusInternalServerError, api.ErrInternal, "%v", err)
	}
	writeJSON(w, e.Status, e.API)
}

// decode reads a JSON body strictly: an unknown field rejects the whole
// request, so a typo never turns into a silently ignored option. An empty
// body is accepted when optional is true.
func decode(r *http.Request, v any, optional bool) error {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err != nil {
		return invalid("", "a JSON body of at most 1MiB", "", "cannot read the request body: %v", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		if optional {
			return nil
		}
		return invalid("", "a JSON object", `{"envelope":{"memory":8589934592}}`, "the request body is empty")
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		field := ""
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			field = te.Field
		} else if rest, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			field = strings.Trim(rest, `"`)
		}
		return invalid(field, "the fields and types defined in api/openapi.yaml", "", "bad request body: %v", err)
	}
	if dec.More() {
		return invalid("", "a single JSON object", "", "unexpected data after the JSON object")
	}
	return nil
}

func leaseID(r *http.Request) api.LeaseID { return api.LeaseID(r.PathValue("id")) }

func (s *Server) getHost(w http.ResponseWriter, r *http.Request, c Caller) error {
	writeJSON(w, http.StatusOK, s.m.Host())
	return nil
}

func (s *Server) createLease(w http.ResponseWriter, r *http.Request, c Caller) error {
	var req api.AcquireRequest
	if err := decode(r, &req, false); err != nil {
		return err
	}
	ls, err := s.m.Acquire(r.Context(), c, req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, ls)
	return nil
}

func (s *Server) listLeases(w http.ResponseWriter, r *http.Request, c Caller) error {
	leases, err := s.m.List(r.Context(), c, api.LeaseID(r.URL.Query().Get("lead")))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, api.LeaseList{Leases: leases})
	return nil
}

func (s *Server) getLease(w http.ResponseWriter, r *http.Request, c Caller) error {
	ls, err := s.m.Get(r.Context(), c, leaseID(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, ls)
	return nil
}

func (s *Server) resizeLease(w http.ResponseWriter, r *http.Request, c Caller) error {
	var p api.EnvelopePatch
	if err := decode(r, &p, false); err != nil {
		return err
	}
	ls, err := s.m.Resize(r.Context(), c, leaseID(r), p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, ls)
	return nil
}

func (s *Server) getAvailable(w http.ResponseWriter, r *http.Request, c Caller) error {
	a, err := s.m.Available(c, leaseID(r), r.URL.Query()["kind"]...)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, a)
	return nil
}

func (s *Server) spawn(w http.ResponseWriter, r *http.Request, c Caller) error {
	var req api.SpawnRequest
	if err := decode(r, &req, false); err != nil {
		return err
	}
	// The process must outlive this request.
	resp, err := s.m.Spawn(context.WithoutCancel(r.Context()), c, leaseID(r), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) terminate(w http.ResponseWriter, r *http.Request, c Caller) error {
	var req api.TerminateRequest
	if err := decode(r, &req, true); err != nil {
		return err
	}
	t, err := s.m.Terminate(r.Context(), c, leaseID(r), req)
	if err != nil {
		return err
	}
	if req.DryRun {
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	writeJSON(w, http.StatusOK, t)
	return nil
}

func (s *Server) kill(w http.ResponseWriter, r *http.Request, c Caller) error {
	var req api.KillRequest
	if err := decode(r, &req, true); err != nil {
		return err
	}
	t, err := s.m.Kill(r.Context(), c, leaseID(r), req)
	if err != nil {
		return err
	}
	if req.DryRun {
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	writeJSON(w, http.StatusOK, t)
	return nil
}

func (s *Server) waitChildren(w http.ResponseWriter, r *http.Request, c Caller) error {
	var req api.WaitChildrenRequest
	if err := decode(r, &req, true); err != nil {
		return err
	}
	resp, err := s.m.WaitChildren(r.Context(), c, leaseID(r), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// String makes a route readable in test failures.
func (r Route) String() string { return fmt.Sprintf("%s %s %s", r.ID, r.Method, r.Path) }

func (s *Server) getTrace(w http.ResponseWriter, r *http.Request, c Caller) error {
	tr, err := s.m.Trace(c, leaseID(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, tr)
	return nil
}

func (s *Server) getMetrics(w http.ResponseWriter, r *http.Request, c Caller) error {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, s.m.MetricsText())
	return nil
}

// streamEvents sends the events the caller may see as server-sent events,
// one JSON object per "data:" line, until the client goes away.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, c Caller) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return apiErr(http.StatusInternalServerError, api.ErrInternal, "this connection cannot stream")
	}
	events, stop := s.m.Events(c)
	defer stop()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": subscribed\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case e, ok := <-events:
			if !ok {
				return nil
			}
			data, err := json.Marshal(e)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, data); err != nil {
				return nil
			}
			flusher.Flush()
		}
	}
}

func (s *Server) notify(w http.ResponseWriter, r *http.Request, c Caller) error {
	var req api.NotifyRequest
	if err := decode(r, &req, false); err != nil {
		return err
	}
	if err := s.m.Notify(c, leaseID(r), req); err != nil {
		return err
	}
	w.WriteHeader(http.StatusAccepted)
	return nil
}

func (s *Server) reportProgress(w http.ResponseWriter, r *http.Request, c Caller) error {
	var req api.ProgressRequest
	if err := decode(r, &req, false); err != nil {
		return err
	}
	if err := s.m.Progress(c, leaseID(r), req); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) requestResize(w http.ResponseWriter, r *http.Request, c Caller) error {
	var req api.ResizeAsk
	if err := decode(r, &req, false); err != nil {
		return err
	}
	if err := s.m.RequestResize(c, leaseID(r), req); err != nil {
		return err
	}
	w.WriteHeader(http.StatusAccepted)
	return nil
}

// streamInbox sends a lease its control messages as server-sent events:
// the backlog first, then each new message, until the lease ends or the
// client goes away.
func (s *Server) streamInbox(w http.ResponseWriter, r *http.Request, c Caller) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return apiErr(http.StatusInternalServerError, api.ErrInternal, "this connection cannot stream")
	}
	msgs, stop, err := s.m.Inbox(c, leaseID(r))
	if err != nil {
		return err
	}
	defer stop()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": subscribed\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case msg, ok := <-msgs:
			if !ok {
				return nil
			}
			data, err := json.Marshal(msg)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.Type, data); err != nil {
				return nil
			}
			flusher.Flush()
		}
	}
}

// AttachProtocol is the Upgrade token of the lead's terminal relay.
const AttachProtocol = "subactl-pty"

// attach upgrades the connection to the raw terminal relay.
func (s *Server) attach(w http.ResponseWriter, r *http.Request, c Caller) error {
	if !strings.EqualFold(r.Header.Get("Upgrade"), AttachProtocol) {
		return invalid("Upgrade", "the header Upgrade: "+AttachProtocol, AttachProtocol, "attach is a protocol upgrade, not a plain request")
	}
	if _, err := s.m.attachable(c, leaseID(r)); err != nil {
		return err
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return apiErr(http.StatusInternalServerError, api.ErrInternal, "this connection cannot be upgraded")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return apiErr(http.StatusInternalServerError, api.ErrInternal, "upgrade: %v", err)
	}
	defer conn.Close()
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + AttachProtocol + "\r\nConnection: Upgrade\r\n\r\n")
	_ = rw.Flush()
	// Read through the buffered reader, which may already hold input, and
	// write straight to the connection.
	_ = s.m.Attach(c, leaseID(r), struct {
		io.Reader
		io.Writer
	}{rw.Reader, conn})
	return nil
}

// AdminGroup is the group whose members may see and end every lead.
const AdminGroup = "subactl-admin"

// inAdminGroup reports whether the user is a member of AdminGroup. A
// machine without that group has no admins besides root.
func inAdminGroup(uid int) bool {
	g, err := user.LookupGroup(AdminGroup)
	if err != nil {
		return false
	}
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return false
	}
	gids, err := u.GroupIds()
	if err != nil {
		return false
	}
	for _, id := range gids {
		if id == g.Gid {
			return true
		}
	}
	return false
}
