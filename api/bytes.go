package api

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseBytes parses a size such as "24G", "512M", "3GiB" or "1048576".
// Suffixes are binary: K, M, G and T mean KiB, MiB, GiB and TiB.
func ParseBytes(s string) (int64, error) {
	t := strings.TrimSpace(s)
	u := strings.ToUpper(t)
	u = strings.TrimSuffix(u, "B")
	u = strings.TrimSuffix(u, "I")
	shift := 0
	if n := len(u); n > 0 {
		switch u[n-1] {
		case 'K':
			shift = 10
		case 'M':
			shift = 20
		case 'G':
			shift = 30
		case 'T':
			shift = 40
		}
		if shift != 0 {
			u = u[:n-1]
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(u), 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q: want a number with an optional K, M, G or T suffix, e.g. 8G", s)
	}
	v := f * float64(int64(1)<<shift)
	if v >= float64(1<<62) {
		return 0, fmt.Errorf("invalid size %q: too large", s)
	}
	return int64(v), nil
}

// FormatBytes renders n with a binary unit, e.g. "3.02GiB".
func FormatBytes(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return strconv.FormatInt(n, 10) + "B"
	}
	return strings.TrimSuffix(strings.TrimRight(strconv.FormatFloat(f, 'f', 2, 64), "0"), ".") + units[i]
}
