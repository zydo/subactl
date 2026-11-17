package daemon

import (
	"net"

	"golang.org/x/sys/unix"
)

// soPeerPidfd is SO_PEERPIDFD (Linux 6.5): a pidfd of the process at the
// other end of a Unix socket.
const soPeerPidfd = 77

// peerCred reads the pid and uid of the process at the other end of c,
// and, where the kernel offers it, a pidfd of that process. The pidfd is
// -1 on older kernels. The caller closes it.
func peerCred(c *net.UnixConn) (pid, uid, pidfd int, err error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, 0, -1, err
	}
	var cred *unix.Ucred
	var cerr error
	pidfd = -1
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if cerr != nil {
			return
		}
		if p, perr := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, soPeerPidfd); perr == nil {
			pidfd = p
		}
	}); err != nil {
		return 0, 0, -1, err
	}
	if cerr != nil {
		return 0, 0, -1, cerr
	}
	return int(cred.Pid), int(cred.Uid), pidfd, nil
}

// exited reports whether the process behind a pidfd has exited: a pidfd
// becomes readable when it does.
func exited(pidfd int) bool {
	fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 0)
	return err == nil && n > 0
}

func closeFD(fd int) { _ = unix.Close(fd) }

// protectSelf makes the daemon the last thing the kernel's global OOM
// killer would choose. The shim sets each agent's own, higher, score.
func protectSelf() error {
	return writeFile("/proc/self/oom_score_adj", "-1000")
}
