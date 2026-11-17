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
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

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
	http   *http.Client
	socket string
}

// New returns a client for the daemon listening on the Unix socket at path.
func New(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if err := api.CheckSocketPath(socket); err != nil {
				return nil, err
			}
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{http: &http.Client{Transport: tr}, socket: socket}
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
	opAttach       = operation{"attach", "GET", "/v1/leases/{id}/attach"}

	// operations lists what the client implements; the conformance test
	// compares it with the contract.
	operations = []operation{
		opGetHost, opCreateLease, opListLeases, opGetLease, opResizeLease,
		opGetAvailable, opSpawn, opTerminate, opKill, opWaitChildren,
		opGetTrace, opStreamEvents, opGetMetrics,
		opNotify, opProgress, opResizeAsk, opStreamInbox, opAttach,
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
	if out == nil || len(data) == 0 { // 204: a dry run, or nothing to return
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

// TraceTree returns the timeline of a lease with those of its whole
// subtree, ended leases included, as children.
func (c *Client) TraceTree(ctx context.Context, id api.LeaseID) (api.Trace, error) {
	var out api.Trace
	return out, c.do(ctx, opGetTrace, id, url.Values{"tree": {"true"}}, nil, &out)
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
func (c *Client) Kill(ctx context.Context, id api.LeaseID, opts ...api.KillRequest) (api.Termination, error) {
	var out api.Termination
	var body any
	if len(opts) > 0 {
		body = opts[0]
	}
	return out, c.do(ctx, opKill, id, nil, body, &out)
}

// WaitChildren waits for the subtree of a lease to end.
func (c *Client) WaitChildren(ctx context.Context, id api.LeaseID, req api.WaitChildrenRequest) (api.WaitChildrenResponse, error) {
	var out api.WaitChildrenResponse
	return out, c.do(ctx, opWaitChildren, id, nil, req, &out)
}

// AttachProtocol is the Upgrade token of the lead's terminal relay.
const AttachProtocol = "subactl-pty"

// Terminal is an attached lead terminal. Reads yield the terminal's raw
// output; input and window sizes are sent with Type and Resize.
type Terminal struct {
	conn net.Conn
	r    *bufio.Reader
	mu   sync.Mutex
}

// Read returns terminal output. It ends with io.EOF when the lead ends.
func (t *Terminal) Read(p []byte) (int, error) { return t.r.Read(p) }

// Close detaches. The lead keeps running for as long as its holder lives.
func (t *Terminal) Close() error { return t.conn.Close() }

func (t *Terminal) frame(typ byte, payload []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	hdr := [5]byte{typ}
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	_, err := t.conn.Write(append(hdr[:], payload...))
	return err
}

// Type sends what the user typed.
func (t *Terminal) Type(p []byte) error { return t.frame(0, p) }

// Resize tells the lead the size of the user's terminal.
func (t *Terminal) Resize(rows, cols uint16) error {
	var b [4]byte
	binary.BigEndian.PutUint16(b[:2], rows)
	binary.BigEndian.PutUint16(b[2:], cols)
	return t.frame(1, b[:])
}

// Attach connects to the terminal of a lead that was acquired with a tty.
func (c *Client) Attach(ctx context.Context, id api.LeaseID) (*Terminal, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, fmt.Errorf("subactld: %w", err)
	}
	path := strings.ReplaceAll(opAttach.path, "{id}", url.PathEscape(string(id)))
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: subactld\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", path, AttachProtocol); err != nil {
		conn.Close()
		return nil, fmt.Errorf("subactld: %w", err)
	}
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("subactld: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer conn.Close()
		data, _ := io.ReadAll(resp.Body)
		e := &api.Error{}
		if json.Unmarshal(data, e) != nil || e.Code == "" {
			e = &api.Error{Code: api.ErrInternal, Message: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))}
		}
		return nil, e
	}
	return &Terminal{conn: conn, r: r}, nil
}
