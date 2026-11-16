package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
		{ID: "attach", Method: "GET", Path: "/v1/leases/{id}/attach", Pending: "M4"},
		{ID: "requestResize", Method: "POST", Path: "/v1/leases/{id}/resize-request", Pending: "M4"},
		{ID: "notify", Method: "POST", Path: "/v1/leases/{id}/notify", Pending: "M4"},
		{ID: "reportProgress", Method: "POST", Path: "/v1/leases/{id}/progress", Pending: "M4"},
		{ID: "getTrace", Method: "GET", Path: "/v1/leases/{id}/trace", Pending: "M3"},
		{ID: "streamInbox", Method: "GET", Path: "/v1/leases/{id}/inbox", Pending: "M4"},
		{ID: "streamEvents", Method: "GET", Path: "/v1/events", Pending: "M3"},
		{ID: "getMetrics", Method: "GET", Path: "/metrics", Pending: "M3"},
	}
}

// NewServer wires the routes. identify may be nil, in which case identity
// comes from the peer credentials of the Unix socket (see ConnContext).
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
	pid, uid int
	err      error
}

// ConnContext records the peer credentials of each Unix connection. Set it
// as http.Server.ConnContext.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return context.WithValue(ctx, peerKey{}, peer{err: errors.New("not a Unix socket connection")})
	}
	pid, uid, err := peerCred(uc)
	return context.WithValue(ctx, peerKey{}, peer{pid, uid, err})
}

// peerIdentity maps the connection's peer pid to the acgroup it is in. A
// process inside an acgroup is that node; any other process is an operator.
// TODO(M4): members of the subactl-admin group are admins too.
func (s *Server) peerIdentity(r *http.Request) (Caller, error) {
	p, ok := r.Context().Value(peerKey{}).(peer)
	if !ok {
		return Caller{}, errors.New("no peer credentials on this connection")
	}
	if p.err != nil {
		return Caller{}, p.err
	}
	ref, err := s.m.be.Identify(r.Context(), p.pid)
	switch {
	case err == nil:
		return Caller{Lease: ref.Lease, UID: p.uid}, nil
	case errors.Is(err, backend.ErrNotMember):
		return Caller{UID: p.uid, Admin: p.uid == 0}, nil
	}
	return Caller{}, err
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
	a, err := s.m.Available(c, leaseID(r))
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
	writeJSON(w, http.StatusOK, t)
	return nil
}

func (s *Server) kill(w http.ResponseWriter, r *http.Request, c Caller) error {
	t, err := s.m.Kill(r.Context(), c, leaseID(r))
	if err != nil {
		return err
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
