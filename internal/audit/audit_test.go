package audit

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zydo/subactl/api"
)

const mib = int64(1) << 20

func TestWriteReadAndSuggest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit")
	l := Open(dir)
	day1 := time.Date(2026, 11, 15, 23, 59, 59, 0, time.UTC)
	day2 := day1.Add(2 * time.Second)
	l.Write(Record{At: day1, Type: TypeAcquire, Lease: "a", Kind: "worker"})
	l.Write(Record{At: day1, Type: TypeAcquire, Lease: "b", Kind: "worker"})
	l.Write(Record{At: day2, Type: TypeTerminate, Lease: "a", Termination: &api.Termination{Code: 200, Kind: "worker", Peak: 100 * mib, MemBasis: api.BasisWorkingSet}})
	// Twenty peaks of 1..20 x 10MiB for a second kind, one of them on
	// another basis, which must not be mixed in.
	for i := 1; i <= 20; i++ {
		l.Write(Record{At: day2, Type: TypeTerminate, Lease: "r", Termination: &api.Termination{Kind: "renderer", Peak: int64(i) * 10 * mib, MemBasis: api.BasisWorkingSet}})
	}
	l.Write(Record{At: day2, Type: TypeTerminate, Lease: "w", Termination: &api.Termination{Kind: "renderer", Peak: 9000 * mib, MemBasis: api.BasisCommit}})
	l.Close()
	if l.Dropped() != 0 {
		t.Fatalf("dropped %d", l.Dropped())
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 2 {
		t.Fatalf("want one file per day, got %v", files)
	}
	recs, err := Read(dir, "a")
	if err != nil || len(recs) != 2 || recs[0].Type != TypeAcquire || recs[1].Termination.Code != 200 {
		t.Fatalf("read %+v, %v", recs, err)
	}

	s := NewSuggester(512*mib, api.BasisWorkingSet)
	if err := s.Load(dir); err != nil {
		t.Fatal(err)
	}
	// One sample is too few: the default, marked as such.
	if got := s.Suggest("worker"); !got.Default || got.Memory != 512*mib || got.Samples != 1 {
		t.Fatalf("worker: %+v", got)
	}
	// p95 of 10..200MiB is 190MiB; times 1.2 is 228MiB.
	if got := s.Suggest("renderer"); got.Default || got.Memory != 228*mib || got.Samples != 20 || got.MemBasis != api.BasisWorkingSet {
		t.Fatalf("renderer: %+v", got)
	}
	if got := s.Suggest("unknown"); !got.Default || got.Samples != 0 {
		t.Fatalf("unknown: %+v", got)
	}
}

// F32: a log that cannot be written drops and counts, and never blocks.
func TestUnwritableDirDropsWithoutBlocking(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(parent, 0o700)
	l := Open(filepath.Join(parent, "audit"))
	start := time.Now()
	for i := 0; i < 20000; i++ { // far more than the buffer holds
		l.Write(Record{Type: TypeSample, Lease: "x"})
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Write blocked: 20000 writes took %v", took)
	}
	l.Close()
	if l.Dropped() != 20000 {
		t.Fatalf("dropped %d of 20000", l.Dropped())
	}
}
