package capacity

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MeminfoReader reads the whole machine from a meminfo file. It is the
// reader for a host or a VM that subactld has to itself.
type MeminfoReader struct {
	Path string // normally /proc/meminfo
}

// Read returns MemTotal and MemAvailable.
func (r MeminfoReader) Read() (Host, error) {
	f, err := os.Open(r.Path)
	if err != nil {
		return Host{}, err
	}
	defer f.Close()
	var h Host
	found := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			h.Total, found = kb<<10, found+1
		case "MemAvailable:":
			h.Available, found = kb<<10, found+1
		}
	}
	if found != 2 {
		return Host{}, fmt.Errorf("capacity: MemTotal and MemAvailable not both found in %s", r.Path)
	}
	return h, nil
}

// CgroupReader treats an outer cgroup as the machine: its memory.max is
// the total and what its working set leaves is available. It is the reader
// for a daemon that runs inside a capped cgroup, such as a blast chamber,
// where /proc/meminfo would describe the host outside the cap.
type CgroupReader struct {
	Dir string
}

func readInt(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}

// Read returns the cgroup's limit and what is left under it.
func (r CgroupReader) Read() (Host, error) {
	total, err := readInt(filepath.Join(r.Dir, "memory.max"))
	if err != nil {
		return Host{}, fmt.Errorf("capacity: %s has no numeric memory.max (is it \"max\"?): %w", r.Dir, err)
	}
	current, err := readInt(filepath.Join(r.Dir, "memory.current"))
	if err != nil {
		return Host{}, err
	}
	var inactive int64
	if data, err := os.ReadFile(filepath.Join(r.Dir, "memory.stat")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "inactive_file "); ok {
				inactive, _ = strconv.ParseInt(v, 10, 64)
			}
		}
	}
	return Host{Total: total, Available: max(total-max(current-inactive, 0), 0)}, nil
}

// ReaderFunc adapts a function to a Reader.
type ReaderFunc func() (Host, error)

// Read calls the function.
func (f ReaderFunc) Read() (Host, error) { return f() }
