//go:build linux || darwin

// Package pty allocates pseudo-terminals with golang.org/x/sys alone, and
// defines the small framing the lead's terminal relay uses.
//
// A process started by the daemon has no terminal of the user's. For a
// lead, the daemon allocates a pty, gives the slave end to the agent and
// relays the master end to "subactl run -it", as "docker run -it" does.
package pty

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Pair is an open pseudo-terminal.
type Pair struct {
	Master *os.File
	// Slave is the path of the slave device, to be opened by the child.
	Slave string
}

// SetSize sets the terminal size seen by the process on the slave side.
// The slave must have been opened at least once: macOS refuses to size a
// master whose slave was never open.
func SetSize(master *os.File, rows, cols uint16) error {
	return unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
}

// Frames from the client to the daemon. The other direction is the raw
// terminal output, unframed.
const (
	FrameData   = 0 // payload: bytes typed by the user
	FrameResize = 1 // payload: rows, cols as two big-endian uint16
	maxFrame    = 64 << 10
)

// WriteFrame writes one frame: type, length, payload.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > maxFrame {
		return errors.New("pty: frame too large")
	}
	hdr := [5]byte{typ}
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(append(hdr[:], payload...)); err != nil {
		return err
	}
	return nil
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (typ byte, payload []byte, err error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("pty: frame of %d bytes exceeds the limit", n)
	}
	payload = make([]byte, n)
	_, err = io.ReadFull(r, payload)
	return hdr[0], payload, err
}

// ResizePayload encodes a terminal size for a FrameResize.
func ResizePayload(rows, cols uint16) []byte {
	var b [4]byte
	binary.BigEndian.PutUint16(b[:2], rows)
	binary.BigEndian.PutUint16(b[2:], cols)
	return b[:]
}

// ParseResize decodes a FrameResize payload.
func ParseResize(p []byte) (rows, cols uint16, ok bool) {
	if len(p) != 4 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint16(p[:2]), binary.BigEndian.Uint16(p[2:]), true
}
