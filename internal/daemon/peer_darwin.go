package daemon

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerCred reads the pid and uid of the process at the other end of c.
func peerCred(c *net.UnixConn) (pid, uid int, err error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, 0, err
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
		return 0, 0, err
	}
	return pid, uid, cerr
}
