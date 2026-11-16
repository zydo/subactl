package pty

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A shell on the slave side sees a real terminal of the size that was set,
// and what is typed on the master side reaches it.
func TestOpenRelaysAndResizes(t *testing.T) {
	p, err := Open()
	if err != nil {
		t.Skipf("cannot allocate a pty here: %v", err)
	}
	defer p.Master.Close()
	slave, err := os.OpenFile(p.Slave, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetSize(p.Master, 33, 101); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", `test -t 0 && echo is-a-tty; stty size; read line; echo "got:$line"`)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close()

	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(p.Master)
		for sc.Scan() {
			lines <- strings.TrimSpace(sc.Text())
		}
		close(lines)
	}()
	want := func(s string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("the terminal closed before %q", s)
				}
				if l == s {
					return
				}
			case <-deadline:
				t.Fatalf("no line %q within 5s", s)
			}
		}
	}
	want("is-a-tty")
	want("33 101")
	if _, err := p.Master.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	want("got:hello")
	_ = cmd.Wait()
}

func TestFrames(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameData, []byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, FrameResize, ResizePayload(40, 120)); err != nil {
		t.Fatal(err)
	}
	typ, p, err := ReadFrame(&buf)
	if err != nil || typ != FrameData || string(p) != "ls\n" {
		t.Fatalf("%d %q %v", typ, p, err)
	}
	typ, p, err = ReadFrame(&buf)
	rows, cols, ok := ParseResize(p)
	if err != nil || typ != FrameResize || !ok || rows != 40 || cols != 120 {
		t.Fatalf("%d %v %v", typ, p, err)
	}
	if _, _, err := ReadFrame(&buf); err == nil {
		t.Fatal("read a frame from nothing")
	}
	if err := WriteFrame(&buf, FrameData, make([]byte, maxFrame+1)); err == nil {
		t.Fatal("an oversized frame was accepted")
	}
	// A header that claims an oversized payload is refused, not allocated.
	if _, _, err := ReadFrame(bytes.NewReader([]byte{0, 0xff, 0xff, 0xff, 0xff})); err == nil {
		t.Fatal("an oversized frame header was accepted")
	}
	if _, _, ok := ParseResize([]byte{1, 2}); ok {
		t.Fatal("a short resize payload was accepted")
	}
}
