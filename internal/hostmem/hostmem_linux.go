package hostmem

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Total returns MemTotal in bytes.
func Total() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return kb << 10, nil
		}
	}
	return 0, fmt.Errorf("hostmem: MemTotal not found in /proc/meminfo")
}
