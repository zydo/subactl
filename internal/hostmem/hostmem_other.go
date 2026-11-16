//go:build !linux && !darwin

package hostmem

import "errors"

// Total is not implemented on this platform.
func Total() (int64, error) { return 0, errors.New("hostmem: unsupported platform") }
