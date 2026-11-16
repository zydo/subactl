// Package client is the public Go client of subactld. It is written by
// hand on net/http and held against api/openapi.yaml by a conformance test.
//
// A client carries no credentials. The daemon identifies the caller from
// the socket connection: a process inside a lease is that node, any other
// process is an operator.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/zydo/subactl/api"
)

// DefaultSocket is where subactld listens unless told otherwise.
const DefaultSocket = "/run/subactl/subactl.sock"

// SocketFromEnv returns $SUBACTL_SOCK, or DefaultSocket when it is unset.
func SocketFromEnv() string {
	if s := os.Getenv("SUBACTL_SOCK"); s != "" {
		return s
	}
	return DefaultSocket
}

// Client talks to one subactld.
type Client struct {
	http *http.Client
}

// New returns a client for the daemon listening on the Unix socket at path.
func New(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{http: &http.Client{Transport: tr}}
}

// operation is one operation of the contract.
type operation struct {
	id, method, path string
}

var (
	opGetHost      = operation{"getHost", "GET", "/v1/host"}
	opCreateLease  = operation{"createLease", "POST", "/v1/leases"}
	opListLeases   = operation{"listLeases", "GET", "/v1/leases"}
	opGetLease     = operation{"getLease", "GET", "/v1/leases/{id}"}
	opResizeLease  = operation{"resizeLease", "PATCH", "/v1/leases/{id}"}
	opGetAvailable = operation{"getAvailable", "GET", "/v1/leases/{id}/available"}
	opSpawn        = operation{"spawn", "POST", "/v1/leases/{id}/spawn"}
	opTerminate    = operation{"terminate", "POST", "/v1/leases/{id}/terminate"}
	opKill         = operation{"kill", "POST", "/v1/leases/{id}/kill"}
	opWaitChildren = operation{"waitChildren", "POST", "/v1/leases/{id}/wait-children"}
	opGetTrace     = operation{"getTrace", "GET", "/v1/leases/{id}/trace"}
	opStreamEvents = operation{"streamEvents", "GET", "/v1/events"}
	opGetMetrics   = operation{"getMetrics", "GET", "/metrics"}
	opNotify       = operation{"notify", "POST", "/v1/leases/{id}/notify"}
	opProgress     = operation{"reportProgress", "POST", "/v1/leases/{id}/progress"}
	opResizeAsk    = operation{"requestResize", "POST", "/v1/leases/{id}/resize-request"}
	opStreamInbox  = operation{"streamInbox", "GET", "/v1/leases/{id}/inbox"}

	// operations lists what the client implements; the conformance test
	// compares it with the contract.
	operations = []operation{
		opGetHost, opCreateLease, opListLeases, opGetLease, opResizeLease,
		opGetAvailable, opSpawn, opTerminate, opKill, opWaitChildren,
		opGetTrace, opStreamEvents, opGetMetrics,
		opNotify, opProgress, opResizeAsk, opStreamInbox,
	}
)

func (c *Client) do(ctx context.Context, op operation, id api.LeaseID, query url.Values, in, out any) error {
	path := strings.ReplaceAll(op.path, "{id}", url.PathEscape(string(id)))
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, op.method, "http://subactld"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("subactld: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("subactld: %w", err)
	}
	if resp.StatusCode >= 300 {
		e := &api.Error{}
		if json.Unmarshal(data, e) != nil || e.Code == "" {
			e = &api.Error{Code: api.ErrInternal, Message: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))}
		}
		return e
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Host returns the machine-wide view.
func (c *Client) Host(ctx context.Context) (api.Host, error) {
	var out api.Host
	return out, c.do(ctx, opGetHost, "", nil, nil, &out)
}

// Acquire asks for a lease. A refusal is an *api.Error whose Reason says why.
func (c *Client) Acquire(ctx context.Context, req api.AcquireRequest) (api.Lease, error) {
	var out api.Lease
	return out, c.do(ctx, opCreateLease, "", nil, req, &out)
}

// Leases lists the trees visible to the caller. lead may be empty.
func (c *Client) Leases(ctx context.Context, lead api.LeaseID) ([]api.Lease, error) {
	q := url.Values{}
	if lead != "" {
		q.Set("lead", string(lead))
	}
	var out api.LeaseList
	return out.Leases, c.do(ctx, opListLeases, "", q, nil, &out)
}

// Lease returns one lease. id may be api.Self.
func (c *Client) Lease(ctx context.Context, id api.LeaseID) (api.Lease, error) {
	var out api.Lease
	return out, c.do(ctx, opGetLease, id, nil, nil, &out)
}

// Resize changes the envelope of a lease in the caller's subtree.
func (c *Client) Resize(ctx context.Context, id api.LeaseID, p api.EnvelopePatch) (api.Lease, error) {
	var out api.Lease
	return out, c.do(ctx, opResizeLease, id, nil, p, &out)
}

// Available returns the remaining budget of a subtree. id may be api.Self.
// Each kind named is included in the suggestions even without history.
func (c *Client) Available(ctx context.Context, id api.LeaseID, kinds ...string) (api.Available, error) {
	var out api.Available
	return out, c.do(ctx, opGetAvailable, id, url.Values{"kind": kinds}, nil, &out)
}

// Trace returns the timeline of a lease: usage series, events, termination.
func (c *Client) Trace(ctx context.Context, id api.LeaseID) (api.Trace, error) {
	var out api.Trace
	return out, c.do(ctx, opGetTrace, id, nil, nil, &out)
}

// Metrics returns the Prometheus metrics as text.
func (c *Client) Metrics(ctx context.Context) (string, error) {
	resp, err := c.open(ctx, opGetMetrics, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return string(data), err
}

// Events streams the events the caller may see to fn until ctx ends, the
// daemon closes the stream, or fn returns an error, which Events returns.
func (c *Client) Events(ctx context.Context, fn func(api.Event) error) error {
	return stream(ctx, c, opStreamEvents, "", fn)
}

// Inbox streams the control messages of a lease to fn: what is queued,
// then what arrives. Only the lease itself may read its inbox, so id is
// normally api.Self. It returns when ctx ends, when the lease ends, or
// when fn returns an error.
func (c *Client) Inbox(ctx context.Context, id api.LeaseID, fn func(api.InboxMessage) error) error {
	return stream(ctx, c, opStreamInbox, id, fn)
}

// Notify puts a control message in a descendant's inbox.
func (c *Client) Notify(ctx context.Context, id api.LeaseID, req api.NotifyRequest) error {
	return c.do(ctx, opNotify, id, nil, req, nil)
}

// Progress reports a beacon for the caller's own lease.
func (c *Client) Progress(ctx context.Context, id api.LeaseID, req api.ProgressRequest) error {
	return c.do(ctx, opProgress, id, nil, req, nil)
}

// RequestResize asks the caller's parent for a larger envelope.
func (c *Client) RequestResize(ctx context.Context, id api.LeaseID, req api.ResizeAsk) error {
	return c.do(ctx, opResizeAsk, id, nil, req, nil)
}

// stream reads a server-sent event stream of JSON objects of type T.
func stream[T any](ctx context.Context, c *Client, op operation, id api.LeaseID, fn func(T) error) error {
	resp, err := c.open(ctx, op, id)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue // comments, event names and blank separators
		}
		var v T
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			return fmt.Errorf("subactld: bad stream entry: %w", err)
		}
		if err := fn(v); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return sc.Err()
}

// open starts a request whose body the caller reads itself.
func (c *Client) open(ctx context.Context, op operation, id api.LeaseID) (*http.Response, error) {
	path := strings.ReplaceAll(op.path, "{id}", url.PathEscape(string(id)))
	req, err := http.NewRequestWithContext(ctx, op.method, "http://subactld"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("subactld: %w", err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		e := &api.Error{}
		if json.Unmarshal(data, e) != nil || e.Code == "" {
			e = &api.Error{Code: api.ErrInternal, Message: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))}
		}
		return nil, e
	}
	return resp, nil
}

// Spawn starts the main process of an admitted lease.
func (c *Client) Spawn(ctx context.Context, id api.LeaseID, req api.SpawnRequest) (api.SpawnResponse, error) {
	var out api.SpawnResponse
	return out, c.do(ctx, opSpawn, id, nil, req, &out)
}

// Terminate ends a lease gracefully and returns its termination record.
func (c *Client) Terminate(ctx context.Context, id api.LeaseID, req api.TerminateRequest) (api.Termination, error) {
	var out api.Termination
	return out, c.do(ctx, opTerminate, id, nil, req, &out)
}

// Kill ends a lease and its subtree at once.
func (c *Client) Kill(ctx context.Context, id api.LeaseID) (api.Termination, error) {
	var out api.Termination
	return out, c.do(ctx, opKill, id, nil, nil, &out)
}

// WaitChildren waits for the subtree of a lease to end.
func (c *Client) WaitChildren(ctx context.Context, id api.LeaseID, req api.WaitChildrenRequest) (api.WaitChildrenResponse, error) {
	var out api.WaitChildrenResponse
	return out, c.do(ctx, opWaitChildren, id, nil, req, &out)
}
