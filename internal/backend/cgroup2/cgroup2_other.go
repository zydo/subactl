//go:build !linux

// Package cgroup2 is the Linux enforcement backend. On other platforms it
// only reports that it is unavailable.
package cgroup2

import (
	"errors"

	"github.com/zydo/subactl/internal/backend"
)

// ShimArg is the hidden first argument of the pre-exec shim.
const ShimArg = "__exec"

// Config configures the backend.
type Config struct {
	Base, StateDir, Socket, Shim string
}

// New fails: cgroup v2 exists only on Linux.
func New(Config) (backend.Backend, error) {
	return nil, errors.New("cgroup2: this backend needs Linux with cgroup v2")
}

// ShimMain fails: there is no shim outside Linux.
func ShimMain([]string) error {
	return errors.New("cgroup2: the pre-exec shim needs Linux")
}
