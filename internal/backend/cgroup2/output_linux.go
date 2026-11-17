package cgroup2

import (
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// output carries a lease's stdout and stderr from the agent to its files
// in the scratch directory, through pipes that the daemon reads.
//
// The agent never writes those files itself, for two reasons. Written
// output sits in the page cache, which counts against the writer's memory:
// a command that floods its output would fill its lease's memory and be
// killed as over its memory limit (413) when it is over its output limit
// (414). And once the limit is passed the daemon stops reading, so the
// writer blocks on a full pipe: the overrun is at most one read, not
// everything written until the next check.
type output struct {
	limit atomic.Int64 // bytes; 0 is no limit
	read  atomic.Int64 // bytes taken from the pipes
	done  chan struct{}
	once  sync.Once
	wg    sync.WaitGroup
}

func newOutput(limit int64) *output {
	o := &output{done: make(chan struct{})}
	o.limit.Store(limit)
	return o
}

// carry copies one pipe into one file until the writers are gone, the
// output is closed, or the limit is passed.
func (o *output) carry(r *os.File, w *os.File) {
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		defer r.Close()
		defer w.Close()
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				total := o.read.Add(int64(n))
				limit := o.limit.Load()
				keep := n
				if limit > 0 && total > limit {
					// Write up to the limit, keep the count of what was
					// read, and stop reading: the writer now blocks until
					// the daemon ends the lease.
					keep = max(0, n-int(total-limit))
					_, _ = w.Write(buf[:keep])
					<-o.done
					return
				}
				if _, werr := w.Write(buf[:keep]); werr != nil {
					_, _ = io.Copy(io.Discard, r) // keep the writer from blocking
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
}

// close stops the copying and waits for it. Calling it again is harmless.
func (o *output) close() {
	o.once.Do(func() { close(o.done) })
	o.wg.Wait()
}
