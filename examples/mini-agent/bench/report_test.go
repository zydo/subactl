package bench

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeGroup lays out one group's results as bench run does.
func writeGroup(t *testing.T, dir, name string, runs []Summary) {
	t.Helper()
	for i := range runs {
		runs[i].Group, runs[i].Run, runs[i].ChamberBytes = name, i+1, 12<<30
		rd := filepath.Join(dir, name, fmt.Sprintf("run-%d", i+1))
		if err := os.MkdirAll(rd, 0o755); err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		for s := 0; s < 60; s++ {
			json.NewEncoder(&b).Encode(Sample{T: float64(s), WS: int64(s) << 27, AgentsWS: int64(s) << 26, Pids: 10, CapHits: runs[i].HostOOM})
		}
		os.WriteFile(filepath.Join(rd, "series.jsonl"), b.Bytes(), 0o644)
	}
	data, _ := json.Marshal(runs)
	os.WriteFile(filepath.Join(dir, name, "summary.json"), data, 0o644)
}

func yes() *bool { v := true; return &v }
func no() *bool  { v := false; return &v }

func TestJudge(t *testing.T) {
	dir := t.TempDir()
	writeGroup(t, dir, "A", []Summary{{OrphansAfter: 30, HostOOM: 400}, {OrphansAfter: 6, HostOOM: 0, Meltdown: true}})
	writeGroup(t, dir, "B", []Summary{{UtilMean: 0.30, WorkLost: 4, SafetyNet: 1, WarnedBeforeNet: yes()}, {UtilMean: 0.34, WorkLost: 6}})
	writeGroup(t, dir, "C", []Summary{{UtilMean: 0.50, WorkLost: 3}, {UtilMean: 0.52, WorkLost: 5, SafetyNet: 1, WarnedBeforeNet: yes()}})
	groups, err := LoadResults(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range Judge(groups) {
		got[v.ID] = v.String()
		if v.Evidence == "" {
			t.Errorf("%s has no evidence", v.ID)
		}
	}
	for _, id := range []string{"H1", "H2", "H3", "H4"} {
		if got[id] != "holds" {
			t.Errorf("%s: %s, want holds", id, got[id])
		}
	}

	// Each hypothesis is refuted by the data that contradicts it.
	refute := func(id, group string, change func(*Summary)) {
		t.Helper()
		g2, _ := LoadResults(dir)
		change(&find(g2, group).Runs[0])
		for _, v := range Judge(g2) {
			if v.ID == id && v.String() != "refuted" {
				t.Errorf("%s was not refuted: %s (%s)", id, v, v.Evidence)
			}
			if v.ID != id && v.String() != "holds" {
				t.Errorf("changing the data for %s also changed %s to %s", id, v.ID, v)
			}
		}
	}
	refute("H1", "B", func(s *Summary) { s.OrphansAfter = 1 })
	refute("H1", "A", func(s *Summary) { s.OrphansAfter = 0 })
	refute("H2", "C", func(s *Summary) { s.HostOOM = 1 })
	refute("H3", "C", func(s *Summary) { s.UtilMean = 0.36 }) // only 12 points above B
	refute("H3", "C", func(s *Summary) { s.WorkLost = 9 })
	refute("H4", "B", func(s *Summary) { s.SafetyNet = 2 })
	refute("H4", "B", func(s *Summary) { s.WarnedBeforeNet = no() })

	// With a group missing nothing is decided, and nothing is claimed.
	for _, v := range Judge(groups[:1]) {
		if v.String() != "undecided" {
			t.Errorf("%s decided from group A alone: %s", v.ID, v)
		}
	}
}

func TestWriteReport(t *testing.T) {
	in, out := t.TempDir(), t.TempDir()
	writeGroup(t, in, "A", []Summary{{OrphansAfter: 30, HostOOM: 400, Completion: 0, WorkLost: 32, UtilMean: 0.8, HostPeakWS: 12}})
	writeGroup(t, in, "B", []Summary{{Completion: 0.4, UtilMean: 0.3, Codes: map[string]int{"200": 30, "413": 2}}})
	writeGroup(t, in, "C", []Summary{{Completion: 0.6, UtilMean: 0.5}})
	groups, err := LoadResults(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteReport(out, groups); err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(filepath.Join(out, "report.md"))
	for _, want := range []string{"| H1 |", "**holds**", "A: without subactl", "200×30 413×2", "memory.svg", "series.parquet"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("report.md lacks %q", want)
		}
	}
	// The charts are well-formed XML with a line per run and a bar per
	// group and metric.
	for name, wantTag := range map[string]string{"memory.svg": "polyline", "metrics.svg": "rect"} {
		data, _ := os.ReadFile(filepath.Join(out, name))
		dec := xml.NewDecoder(bytes.NewReader(data))
		count := 0
		for {
			tok, err := dec.Token()
			if tok == nil {
				if err != nil && err.Error() != "EOF" {
					t.Fatalf("%s is not well-formed: %v", name, err)
				}
				break
			}
			if se, ok := tok.(xml.StartElement); ok && se.Name.Local == wantTag {
				count++
			}
		}
		if count < 3 {
			t.Errorf("%s has %d <%s> elements", name, count, wantTag)
		}
	}
	// The Parquet file: magic at both ends, and a footer whose declared
	// length fits. Its content was checked against pyarrow when the writer
	// was written; a reader is not part of this project.
	pq, _ := os.ReadFile(filepath.Join(out, "series.parquet"))
	if len(pq) < 12 || string(pq[:4]) != "PAR1" || string(pq[len(pq)-4:]) != "PAR1" {
		t.Fatal("series.parquet lacks the Parquet magic")
	}
	footer := int(binary.LittleEndian.Uint32(pq[len(pq)-8:]))
	// 3 groups x 60 rows; the fixed-width columns alone are 6 x 8 + 4 bytes a row.
	if footer <= 0 || footer > len(pq)-12 || len(pq)-footer < 180*52 {
		t.Fatalf("footer of %d bytes in a file of %d", footer, len(pq))
	}
}

func TestParquetRejectsRaggedColumns(t *testing.T) {
	err := WriteParquet(&bytes.Buffer{}, []Column{{Name: "a", Int64s: []int64{1, 2}}, {Name: "b", Doubles: []float64{1}}})
	if err == nil {
		t.Fatal("columns of different lengths were accepted")
	}
}
