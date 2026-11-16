package daemon

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerCred reads the pid and uid of the process at the other end of c.
// TODO(M2): use SO_PEERPIDFD on Linux 6.5+ to close the pid-reuse race.
func peerCred(c *net.UnixConn) (pid, uid int, err error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, 0, err
	}
	if cerr != nil {
		return 0, 0, cerr
	}
	return int(cred.Pid), int(cred.Uid), nil
}
