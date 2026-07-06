package gateway

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	// Two FrameConns over an in-memory pipe: write on one end, read on the other.
	cr, cw := net.Pipe()
	defer cr.Close()
	defer cw.Close()

	writer := NewFrameConn(cw)
	reader := NewFrameConn(cr)

	frames := [][]byte{
		[]byte("a"),
		bytes.Repeat([]byte{0xAB}, 1500), // MTU-sized
		[]byte("the quick brown frame"),
	}

	go func() {
		for _, f := range frames {
			if err := writer.WriteFrame(f); err != nil {
				t.Errorf("WriteFrame: %v", err)
				return
			}
		}
	}()

	for i, want := range frames {
		got, err := reader.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d ReadFrame: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d = %q, want %q", i, got, want)
		}
	}
}

func TestFrameWireFormat(t *testing.T) {
	// The bytes on the wire must be [4-byte BE len][frame] — the exact format
	// libkrun's unixstream backend expects.
	var buf bytes.Buffer
	fc := NewFrameConn(&buf)
	frame := []byte{0xde, 0xad, 0xbe, 0xef}
	if err := fc.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	out := buf.Bytes()
	if len(out) != frameHeaderLen+len(frame) {
		t.Fatalf("wire length = %d, want %d", len(out), frameHeaderLen+len(frame))
	}
	if n := binary.BigEndian.Uint32(out[:4]); n != uint32(len(frame)) {
		t.Fatalf("length prefix = %d, want %d", n, len(frame))
	}
	if !bytes.Equal(out[4:], frame) {
		t.Fatalf("body = %x, want %x", out[4:], frame)
	}
}

func TestFrameReadHandlesPartialAndEOF(t *testing.T) {
	// A body split across two reads (io.ReadFull must reassemble); then a clean EOF.
	frame := bytes.Repeat([]byte{0x7}, 1000)
	var wire bytes.Buffer
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(frame)))
	wire.Write(hdr)
	wire.Write(frame)

	fc := NewFrameConn(rwPair{Reader: &slowReader{data: wire.Bytes(), chunk: 7}, Writer: io.Discard})
	got, err := fc.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame across chunks: %v", err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatal("reassembled frame mismatch")
	}
	// Next read hits EOF cleanly.
	if _, err := fc.ReadFrame(); err != io.EOF {
		t.Fatalf("want io.EOF at stream end, got %v", err)
	}
}

func TestFrameRejectsBadLengths(t *testing.T) {
	// Zero-length and oversized prefixes are rejected (desync / abuse guard).
	zero := make([]byte, 4) // len=0
	if _, err := NewFrameConn(rwPair{Reader: bytes.NewReader(zero), Writer: io.Discard}).ReadFrame(); err == nil {
		t.Error("zero-length frame should be rejected")
	}
	big := make([]byte, 4)
	binary.BigEndian.PutUint32(big, maxFrameLen+1)
	if _, err := NewFrameConn(rwPair{Reader: bytes.NewReader(big), Writer: io.Discard}).ReadFrame(); err == nil {
		t.Error("oversized frame should be rejected")
	}

	fc := NewFrameConn(&bytes.Buffer{})
	if err := fc.WriteFrame(nil); err == nil {
		t.Error("empty write should be rejected")
	}
	if err := fc.WriteFrame(bytes.Repeat([]byte{1}, maxFrameLen+1)); err == nil {
		t.Error("oversized write should be rejected")
	}
}

func TestFrameConcurrentWritesDontInterleave(t *testing.T) {
	// Many goroutines writing frames concurrently: every frame on the wire must
	// be intact (len prefix immediately followed by its own body), never spliced.
	var buf syncBuf
	fc := NewFrameConn(rwPair{Reader: nil, Writer: &buf})
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f := bytes.Repeat([]byte{byte(i)}, 64)
			_ = fc.WriteFrame(f)
		}(i)
	}
	wg.Wait()

	// Re-parse the wire: each frame must be exactly 64 bytes of a single value.
	rc := NewFrameConn(rwPair{Reader: bytes.NewReader(buf.Bytes()), Writer: io.Discard})
	seen := 0
	for {
		f, err := rc.ReadFrame()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("parse frame %d: %v", seen, err)
		}
		if len(f) != 64 {
			t.Fatalf("frame %d len=%d, want 64 (interleaved!)", seen, len(f))
		}
		for _, b := range f {
			if b != f[0] {
				t.Fatalf("frame %d has mixed bytes (interleaved write)", seen)
			}
		}
		seen++
	}
	if seen != n {
		t.Fatalf("parsed %d frames, want %d", seen, n)
	}
}

// rwPair adapts a separate reader/writer into an io.ReadWriter for tests.
type rwPair struct {
	io.Reader
	io.Writer
}

// slowReader yields at most `chunk` bytes per Read to exercise io.ReadFull.
type slowReader struct {
	data  []byte
	chunk int
	pos   int
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	n := s.chunk
	if n > len(p) {
		n = len(p)
	}
	if s.pos+n > len(s.data) {
		n = len(s.data) - s.pos
	}
	copy(p, s.data[s.pos:s.pos+n])
	s.pos += n
	return n, nil
}

// syncBuf is a mutex-guarded bytes.Buffer (concurrent writers in the test).
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *syncBuf) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}
