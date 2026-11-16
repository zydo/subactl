// Package telemetry holds the observation side of subactld: the event bus
// and the per-lease ring of usage samples. Nothing here may slow down
// enforcement: every buffer is bounded, and when one is full the data is
// dropped and counted instead of making the publisher wait.
package telemetry

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/zydo/subactl/api"
)

// Event types published by the daemon.
const (
	EventState          = "STATE"           // a lease changed state
	EventTerminated     = "TERMINATED"      // a lease ended; data holds the record
	EventNearLimit      = "NEAR_LIMIT"      // usage reached the warning share of the limit
	EventResized        = "RESIZED"         // an envelope changed
	EventCapacityShrunk = "CAPACITY_SHRUNK" // allocation exceeds capacity, or memory is below the reserve line
	EventEscape         = "ESCAPE"          // a process was found outside its acgroup
)

// Scoped is an event with what is needed to decide who may see it. The
// scope is fixed when the event is published, because the lease may be
// gone by the time a subscriber is asked.
type Scoped struct {
	api.Event
	// Path is the lease and its ancestors, lead last. Empty for host events.
	Path []api.LeaseID
	// OwnerUID is the operator that owns the tree.
	OwnerUID int
}

// Visible reports whether a caller may see the event: a node sees its own
// subtree, an operator sees the trees it owns, and host events go to all.
func (s Scoped) Visible(node api.LeaseID, uid int, admin bool) bool {
	if len(s.Path) == 0 {
		return true
	}
	if node == "" {
		return admin || s.OwnerUID == uid
	}
	for _, id := range s.Path {
		if id == node {
			return true
		}
	}
	return false
}

// Sub is one subscription. Events arrive on C.
type Sub struct {
	C chan Scoped
}

// Bus fans events out to subscribers without ever blocking the publisher.
type Bus struct {
	mu      sync.Mutex
	seq     int64
	subs    map[*Sub]struct{}
	dropped atomic.Int64
}

// NewBus returns an empty bus.
func NewBus() *Bus { return &Bus{subs: map[*Sub]struct{}{}} }

// Publish stamps the event with a sequence number and time and offers it
// to every subscriber. A subscriber whose buffer is full misses it.
func (b *Bus) Publish(e Scoped) Scoped {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	e.Seq, e.At = b.seq, time.Now()
	for s := range b.subs {
		select {
		case s.C <- e:
		default:
			b.dropped.Add(1)
		}
	}
	return e
}

// Subscribe adds a subscriber with a buffer of n events.
func (b *Bus) Subscribe(n int) *Sub {
	s := &Sub{C: make(chan Scoped, n)}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

// Unsubscribe removes a subscriber.
func (b *Bus) Unsubscribe(s *Sub) {
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
}

// Dropped is how many deliveries were skipped because a buffer was full.
func (b *Bus) Dropped() int64 { return b.dropped.Load() }

// Ring keeps the most recent samples of one lease.
type Ring struct {
	buf  []api.Usage
	next int
	full bool
}

// NewRing returns a ring holding up to n samples.
func NewRing(n int) *Ring { return &Ring{buf: make([]api.Usage, n)} }

// Add appends a sample, overwriting the oldest when full.
func (r *Ring) Add(u api.Usage) {
	r.buf[r.next] = u
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// Samples returns the samples, oldest first.
func (r *Ring) Samples() []api.Usage {
	if !r.full {
		return append([]api.Usage{}, r.buf[:r.next]...)
	}
	return append(append([]api.Usage{}, r.buf[r.next:]...), r.buf[:r.next]...)
}

// Rate is the growth of memory in bytes per second over the last few
// samples, or zero when there are too few.
func (r *Ring) Rate() float64 {
	s := r.Samples()
	if len(s) < 2 {
		return 0
	}
	first := s[max(0, len(s)-6)]
	last := s[len(s)-1]
	dt := last.SampledAt.Sub(first.SampledAt).Seconds()
	if dt <= 0 {
		return 0
	}
	return float64(last.Memory-first.Memory) / dt
}
