package daemon

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// kernelAtLeast reports whether the running kernel is major.minor or later.
func kernelAtLeast(major, minor int) bool {
	var u unix.Utsname
	if unix.Uname(&u) != nil {
		return false
	}
	var ma, mi int
	fmt.Sscanf(unix.ByteSliceToString(u.Release[:]), "%d.%d", &ma, &mi)
	return ma > major || (ma == major && mi >= minor)
}

// The pid-reuse guard: on Linux 6.5 and later the peer comes with a pidfd,
// which tells a live caller from one that has exited, so a pid that was
// given to another process meanwhile is never mistaken for the caller.
func TestPeerPidfd(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// A peer that connects and then exits.
	child := exec.Command("python3", "-c", "import socket,sys; s=socket.socket(socket.AF_UNIX); s.connect(sys.argv[1]); sys.stdout.write('c'); sys.stdout.flush(); sys.stdin.read()", sock)
	stdin, _ := child.StdinPipe()
	out, _ := child.StdoutPipe()
	if err := child.Start(); err != nil {
		t.Skipf("python3 is needed to play the peer: %v", err)
	}
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	buf := make([]byte, 1)
	out.Read(buf)

	pid, uid, pidfd, err := peerCred(conn.(*net.UnixConn))
	if err != nil || pid != child.Process.Pid || uid != os.Getuid() {
		t.Fatalf("pid %d (want %d), uid %d, %v", pid, child.Process.Pid, uid, err)
	}
	if !kernelAtLeast(6, 5) {
		if pidfd != -1 {
			t.Fatalf("a pidfd on a kernel without SO_PEERPIDFD: %d", pidfd)
		}
		t.Skip("SO_PEERPIDFD needs Linux 6.5; the pid alone is used here")
	}
	if pidfd < 0 {
		t.Fatal("no pidfd on a kernel that has SO_PEERPIDFD")
	}
	defer closeFD(pidfd)
	if exited(pidfd) {
		t.Fatal("a live peer reads as exited")
	}
	stdin.Close()
	_ = child.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for !exited(pidfd) {
		if time.Now().After(deadline) {
			t.Fatal("the pidfd did not report the peer's exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
