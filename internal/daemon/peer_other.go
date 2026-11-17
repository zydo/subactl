//go:build !linux && !darwin

package daemon

import (
	"errors"
	"net"
)

func peerCred(*net.UnixConn) (pid, uid, pidfd int, err error) {
	return 0, 0, -1, errors.New("peer credentials are not supported on this platform")
}

// This platform has no pidfd: the peer's pid is all there is.
func exited(int) bool { return false }

func closeFD(int) {}

// protectSelf does nothing here: there is no per-process OOM score.
func protectSelf() error { return nil }
