package daemon

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerCred reads the pid and uid of the process at the other end of c.
func peerCred(c *net.UnixConn) (pid, uid, pidfd int, err error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, 0, -1, err
	}
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		var x *unix.Xucred
		if x, cerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED); cerr != nil {
			return
		}
		uid = int(x.Uid)
		pid, cerr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return 0, 0, -1, err
	}
	return pid, uid, -1, cerr
}

// This platform has no pidfd: the peer's pid is all there is.
func exited(int) bool { return false }

func closeFD(int) {}

// protectSelf does nothing here: there is no per-process OOM score.
func protectSelf() error { return nil }
