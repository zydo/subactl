// Package audit is the append-only audit log: one JSON object per line in
// <state>/audit/YYYY-MM-DD.jsonl. It records every state change, resize,
// safety-net action and termination, plus a five-second usage series, and
// serves two readers: the per-lease timeline and the quota suggestions.
//
// Writing never blocks the caller. A full buffer or a failing disk drops
// records and counts them; enforcement goes on regardless.
package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zydo/subactl/api"
)

// Record types.
const (
	TypeAcquire   = "acquire"
	TypeState     = "state"
	TypeResize    = "resize"
	TypeSample    = "sample"
	TypeEvent     = "event"
	TypeTerminate = "terminate"
)

// Record is one line of the log.
type Record struct {
	At    time.Time   `json:"at"`
	Type  string      `json:"type"`
	Lease api.LeaseID `json:"lease,omitempty"`
	// Path is the lease and its ancestors, lead last.
	Path        []api.LeaseID    `json:"path,omitempty"`
	Kind        string           `json:"kind,omitempty"`
	OwnerUID    int              `json:"owner_uid"`
	Caller      string           `json:"caller,omitempty"`
	State       api.State        `json:"state,omitempty"`
	Envelope    *api.Envelope    `json:"envelope,omitempty"`
	Usage       *api.Usage       `json:"usage,omitempty"`
	Event       *api.Event       `json:"event,omitempty"`
	Termination *api.Termination `json:"termination,omitempty"`
}

// Log writes records in the background.
type Log struct {
	dir     string
	ch      chan Record
	done    chan struct{}
	dropped atomic.Int64
	once    sync.Once
}

// Open starts a log in dir, which is created if needed. A directory that
// cannot be written does not fail Open: records are dropped and counted.
func Open(dir string) *Log {
	l := &Log{dir: dir, ch: make(chan Record, 4096), done: make(chan struct{})}
	go l.run()
	return l
}

// Write queues a record. It never blocks.
func (l *Log) Write(r Record) {
	if r.At.IsZero() {
		r.At = time.Now()
	}
	select {
	case l.ch <- r:
	default:
		l.dropped.Add(1)
	}
}

// Dropped is how many records were lost to a full buffer or a write error.
func (l *Log) Dropped() int64 { return l.dropped.Load() }

// Close flushes what is queued and stops the writer.
func (l *Log) Close() {
	l.once.Do(func() {
		close(l.ch)
		<-l.done
	})
}

func (l *Log) run() {
	defer close(l.done)
	var f *os.File
	var w *bufio.Writer
	var day string
	closeFile := func() {
		if f != nil {
			_ = w.Flush()
			_ = f.Close()
			f = nil
		}
	}
	defer closeFile()
	flush := time.NewTicker(time.Second)
	defer flush.Stop()
	for {
		select {
		case r, ok := <-l.ch:
			if !ok {
				return
			}
			if d := r.At.Format("2006-01-02"); d != day || f == nil {
				closeFile()
				day = d
				if err := os.MkdirAll(l.dir, 0o750); err == nil {
					f, _ = os.OpenFile(filepath.Join(l.dir, d+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
				}
				if f == nil {
					l.dropped.Add(1)
					continue
				}
				w = bufio.NewWriter(f)
			}
			line, err := json.Marshal(r)
			if err == nil {
				_, err = w.Write(append(line, '\n'))
			}
			if err != nil {
				l.dropped.Add(1)
				closeFile() // reopen on the next record; the disk may recover
			}
		case <-flush.C:
			if f != nil {
				if err := w.Flush(); err != nil {
					l.dropped.Add(1)
					closeFile()
				}
			}
		}
	}
}

// Read returns the records of one lease, oldest first, from every day's
// file.
func Read(dir string, lease api.LeaseID) ([]Record, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []Record
	for _, name := range files {
		err := scan(name, func(r Record) {
			if r.Lease == lease {
				out = append(out, r)
			}
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func scan(name string, fn func(Record)) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil { // a torn last line is skipped
			fn(r)
		}
	}
	return sc.Err()
}

// Suggester turns the peaks of past leases into initial-quota suggestions.
// It is deterministic: the same history gives the same answer.
type Suggester struct {
	mu sync.Mutex
	// Default is suggested, and marked as such, when a kind has fewer than
	// MinSamples peaks on record.
	Default    int64
	MinSamples int
	Basis      api.MemBasis
	peaks      map[string][]int64
}

// maxPeaks bounds the history kept per kind.
const maxPeaks = 1000

// NewSuggester returns a suggester with no history.
func NewSuggester(def int64, basis api.MemBasis) *Suggester {
	return &Suggester{Default: def, MinSamples: 5, Basis: basis, peaks: map[string][]int64{}}
}

// Add records the peak of one finished lease. Only a lease that ran to
// completion (a 2xx code) is a measurement of what its kind needs. The
// peak of a lease killed at its memory limit is the limit, and the peak of
// one ended early is whatever it had reached: counting either would make
// the suggestion follow the limits instead of the need. In particular,
// with a grow rule each 413 would raise the next suggestion, which would
// raise the next limit, without end.
func (s *Suggester) Add(kind string, peak int64, code int) {
	if kind == "" || peak <= 0 || code < 200 || code > 299 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := append(s.peaks[kind], peak)
	if len(p) > maxPeaks {
		p = p[len(p)-maxPeaks:]
	}
	s.peaks[kind] = p
}

// Load replays the terminations recorded in dir. Only peaks measured on
// the suggester's own basis count: figures on different bases must not mix.
func (s *Suggester) Load(dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, name := range files {
		err := scan(name, func(r Record) {
			if t := r.Termination; r.Type == TypeTerminate && t != nil && t.MemBasis == s.Basis {
				s.Add(t.Kind, t.Peak, t.Code)
			}
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Suggest returns the 95th percentile of the kind's peaks times 1.2.
func (s *Suggester) Suggest(kind string) api.Suggestion {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := append([]int64{}, s.peaks[kind]...)
	out := api.Suggestion{Samples: len(p), MemBasis: s.Basis}
	if len(p) < s.MinSamples {
		out.Memory, out.Default = s.Default, true
		return out
	}
	sort.Slice(p, func(i, j int) bool { return p[i] < p[j] })
	// Nearest-rank percentile: the smallest value with at least 95% at or below it.
	rank := (95*len(p) + 99) / 100
	out.Memory = p[rank-1] * 12 / 10
	return out
}

// Kinds lists the kinds with history, sorted.
func (s *Suggester) Kinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.peaks))
	for k := range s.peaks {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
