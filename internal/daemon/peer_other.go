//go:build !linux && !darwin

package daemon

import (
	"errors"
	"net"
)

func peerCred(*net.UnixConn) (pid, uid int, err error) {
	return 0, 0, errors.New("peer credentials are not supported on this platform")
}
