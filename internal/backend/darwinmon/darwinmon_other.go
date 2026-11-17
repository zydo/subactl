//go:build !darwin

// Package darwinmon is the macOS monitoring backend. On other platforms it
// only reports that it is unavailable.
package darwinmon

import (
	"errors"

	"github.com/zydo/subactl/internal/backend"
)

// Config configures the backend.
type Config struct {
	StateDir, Socket string
}

// New fails: this backend is for macOS.
func New(Config) (backend.Backend, error) {
	return nil, errors.New("darwinmon: this backend needs macOS")
}
