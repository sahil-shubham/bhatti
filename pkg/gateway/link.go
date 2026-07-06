package gateway

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// This file is the link layer between bhatti-netd and the guest's virtio-net
// device. libkrun's unixstream backend (src/devices/src/virtio/net/unixstream.rs)
// frames each ethernet frame on the UDS as a 4-byte big-endian length prefix
// followed by the raw frame — the QEMU `-netdev socket` / gvproxy wire format.
// netd's gVisor-netstack link endpoint reads/writes frames through FrameConn.
//
// Design: DESIGN-bhatti-v2-networking.md §0c (the unified gateway).

const (
	frameHeaderLen = 4          // big-endian u32 length prefix
	maxFrameLen    = 128 * 1024 // sanity cap (jumbo + headroom); reject larger
)

// FrameConn reads/writes length-prefixed ethernet frames over a stream (the
// unixstream UDS to libkrun). Reads are single-goroutine; writes are serialized
// with a mutex so a length prefix can never interleave with another frame's
// bytes (the machinen lesson).
type FrameConn struct {
	r   io.Reader
	w   io.Writer
	wmu sync.Mutex

	hdr []byte // reusable read header buffer
}

// NewFrameConn wraps a stream (typically a *net.UnixConn) as a frame link.
func NewFrameConn(rw io.ReadWriter) *FrameConn {
	return &FrameConn{r: rw, w: rw, hdr: make([]byte, frameHeaderLen)}
}

// ReadFrame reads one ethernet frame (blocking). It returns a freshly allocated
// slice owning exactly the frame bytes. io.EOF is returned verbatim on a clean
// close so callers can stop the read loop.
func (c *FrameConn) ReadFrame() ([]byte, error) {
	if _, err := io.ReadFull(c.r, c.hdr); err != nil {
		return nil, err // includes io.EOF / io.ErrUnexpectedEOF
	}
	n := binary.BigEndian.Uint32(c.hdr)
	if n == 0 {
		return nil, fmt.Errorf("frame: zero-length frame")
	}
	if n > maxFrameLen {
		return nil, fmt.Errorf("frame: length %d exceeds cap %d (desync?)", n, maxFrameLen)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return nil, fmt.Errorf("frame: short read of %d-byte body: %w", n, err)
	}
	return buf, nil
}

// WriteFrame writes one ethernet frame with its length prefix as a single
// serialized operation. frame must be non-empty and within the cap.
func (c *FrameConn) WriteFrame(frame []byte) error {
	if len(frame) == 0 {
		return fmt.Errorf("frame: refusing to write empty frame")
	}
	if len(frame) > maxFrameLen {
		return fmt.Errorf("frame: length %d exceeds cap %d", len(frame), maxFrameLen)
	}
	// One buffer [len||frame] so the prefix and body are a single Write and can't
	// interleave with a concurrent writer.
	out := make([]byte, frameHeaderLen+len(frame))
	binary.BigEndian.PutUint32(out[:frameHeaderLen], uint32(len(frame)))
	copy(out[frameHeaderLen:], frame)

	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.w.Write(out); err != nil {
		return fmt.Errorf("frame: write %d-byte frame: %w", len(frame), err)
	}
	return nil
}
