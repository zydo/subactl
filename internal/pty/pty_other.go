//go:build !linux && !darwin

package pty

import "errors"

// Open is not implemented on this platform.
func Open() (*Pair, error) { return nil, errors.New("pty: unsupported platform") }
