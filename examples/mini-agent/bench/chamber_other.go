//go:build !linux

package bench

import "errors"

// Run needs Linux cgroup v2.
func Run(Options) (*Summary, error) {
	return nil, errors.New("the bench needs Linux with cgroup v2")
}
