// Package bench runs a scenario inside a blast chamber and measures it.
//
// The chamber is an outer cgroup with a hard memory cap. The without
// group is designed to exhaust memory; the cap keeps that inside the
// chamber so the machine survives and the data is complete.
package bench

import (
	"encoding/json"
	"os"
	"time"
)

// Sample is one reading of the chamber, taken every second.
type Sample struct {
	T        float64 `json:"t"`         // seconds since the lead started
	WS       int64   `json:"ws"`        // working set of the whole chamber, bytes
	AgentsWS int64   `json:"agents_ws"` // working set of the agent tree only
	Pids     int64   `json:"pids"`
	OOMKills int64   `json:"oom_kills"` // cumulative
}

// Summary holds the metrics of one run, named as in the design.
type Summary struct {
	Group         string   `json:"group"`
	Run           int      `json:"run"`
	Scenario      string   `json:"scenario"`
	ChamberBytes  int64    `json:"chamber_bytes"`
	MemBasis      string   `json:"mem_basis"`
	HostPeakWS    float64  `json:"host_peak_ws"`    // GiB
	HostOOM       int64    `json:"host_oom"`        // OOM kills in the chamber
	Meltdown      bool     `json:"meltdown"`        // the chamber stopped responding and was killed
	OrphansAfter  int      `json:"orphans_after"`   // processes left 60s after the end
	LeakedWSAfter float64  `json:"leaked_ws_after"` // GiB held by those processes
	Completion    float64  `json:"completion"`      // share of packages upgraded
	Packages      int      `json:"packages"`
	WorkLost      int      `json:"work_lost"` // subtasks killed and redone
	UtilMean      float64  `json:"util_mean"` // mean agent working set / chamber size
	SafetyNet     int      `json:"safety_net_count"`
	Makespan      float64  `json:"makespan"` // minutes
	LeadExit      int      `json:"lead_exit"`
	OrphanSample  []string `json:"orphan_sample,omitempty"` // a few command lines
}

const gib = float64(1 << 30)

// Summarize fills the series-derived metrics of s.
func Summarize(s *Summary, series []Sample) {
	var sum float64
	for _, x := range series {
		if g := float64(x.WS) / gib; g > s.HostPeakWS {
			s.HostPeakWS = g
		}
		sum += float64(x.AgentsWS)
		if x.OOMKills > s.HostOOM {
			s.HostOOM = x.OOMKills
		}
	}
	if len(series) > 0 && s.ChamberBytes > 0 {
		s.UtilMean = sum / float64(len(series)) / float64(s.ChamberBytes)
	}
}

// SeriesWriter appends samples to a JSONL file.
type SeriesWriter struct {
	f   *os.File
	enc *json.Encoder
	All []Sample
}

// NewSeriesWriter creates the file.
func NewSeriesWriter(path string) (*SeriesWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &SeriesWriter{f: f, enc: json.NewEncoder(f)}, nil
}

// Add records one sample.
func (w *SeriesWriter) Add(s Sample) {
	w.All = append(w.All, s)
	_ = w.enc.Encode(s)
}

// Close closes the file.
func (w *SeriesWriter) Close() error { return w.f.Close() }

// Hog allocates memory up to bytes over ramp, holds it for hold, then
// returns. It is the external interference: a process that is not an agent.
func Hog(bytes int64, ramp, hold time.Duration) {
	const chunk = 16 << 20
	n := int(bytes / chunk)
	held := make([][]byte, 0, n)
	pause := time.Duration(0)
	if n > 0 {
		pause = ramp / time.Duration(n)
	}
	for range n {
		buf := make([]byte, chunk)
		for j := 0; j < len(buf); j += 4096 {
			buf[j] = 1
		}
		held = append(held, buf)
		time.Sleep(pause)
	}
	time.Sleep(hold)
	_ = held
}
