package daemon

import (
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/internal/pty"
)

// ttyRelay carries a lead's terminal between its pty and, when one is
// attached, the client. Output is always drained, attached or not, so the
// agent never blocks on a full terminal; what nobody is attached to see is
// dropped. The bytes relayed count against the lease's output limit.
type ttyRelay struct {
	master *os.File
	bytes  atomic.Int64

	mu     sync.Mutex
	client io.Writer
	closed chan struct{} // closed when the agent's side of the pty is gone
}

func newTTYRelay(master *os.File) *ttyRelay {
	r := &ttyRelay{master: master, closed: make(chan struct{})}
	go func() {
		defer close(r.closed)
		buf := make([]byte, 32<<10)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				r.bytes.Add(int64(n))
				r.mu.Lock()
				if r.client != nil {
					if _, werr := r.client.Write(buf[:n]); werr != nil {
						r.client = nil
					}
				}
				r.mu.Unlock()
			}
			if err != nil {
				return // EIO: every slave end is closed
			}
		}
	}()
	return r
}

func (r *ttyRelay) close() { _ = r.master.Close() }

// Attach relays a lead's terminal over conn until the client goes away or
// the lead ends. The client sends frames (typed input, window size); the
// daemon sends the terminal's raw output. Only the operator who owns the
// lead may attach, and only one client at a time.
func (m *Manager) Attach(c Caller, id api.LeaseID, conn io.ReadWriter) error {
	rt, err := m.attachable(c, id)
	if err != nil {
		return err
	}
	r := rt.tty
	r.mu.Lock()
	if r.client != nil {
		r.mu.Unlock()
		return apiErr(http.StatusConflict, api.ErrConflict, "another client is attached to lease %s", id)
	}
	r.client = conn
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.client = nil
		r.mu.Unlock()
	}()
	input := make(chan error, 1)
	go func() {
		for {
			typ, payload, err := pty.ReadFrame(conn)
			if err != nil {
				input <- err
				return
			}
			switch typ {
			case pty.FrameData:
				if _, err := r.master.Write(payload); err != nil {
					input <- err
					return
				}
			case pty.FrameResize:
				if rows, cols, ok := pty.ParseResize(payload); ok {
					_ = pty.SetSize(r.master, rows, cols)
				}
			}
		}
	}()
	select {
	case <-input: // the client went away; the lead lives on with its holder
	case <-r.closed:
	case <-rt.done:
	}
	return nil
}

// attachable checks that the caller may attach to the lease and returns
// its runtime.
func (m *Manager) attachable(c Caller, id api.LeaseID) (*runtime, error) {
	ls, err := m.resolve(c, id)
	if err != nil {
		return nil, err
	}
	if c.Lease != "" {
		return nil, apiErr(http.StatusForbidden, api.ErrOutOfSubtree, "a terminal is attached by the operator who started the lead, not from inside a lease")
	}
	rt := m.runtime(ls.ID)
	if rt == nil || rt.tty == nil {
		return nil, apiErr(http.StatusConflict, api.ErrConflict, "lease %s has no terminal: it was not started with a tty, or it has ended", ls.ID)
	}
	return rt, nil
}
