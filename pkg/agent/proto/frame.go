package proto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// WriteFrame writes a single frame: [4-byte length BE][1-byte type][payload].
// Length = 1 + len(payload) (excludes the 4-byte length prefix itself).
// The entire frame is assembled into one buffer before writing to prevent
// interleaved partial frames when multiple goroutines write concurrently.
// Returns error if the frame length exceeds MaxFrameSize.
func WriteFrame(w io.Writer, msgType byte, payload []byte) error {
	frameLen := 1 + len(payload)
	if frameLen > MaxFrameSize {
		return fmt.Errorf("frame too large: %d > %d", frameLen, MaxFrameSize)
	}

	buf := make([]byte, 4+frameLen)
	binary.BigEndian.PutUint32(buf[0:4], uint32(frameLen))
	buf[4] = msgType
	copy(buf[5:], payload)

	_, err := w.Write(buf)
	return err
}

// ReadFrame reads one complete frame from r.
// Returns io.EOF on a clean end-of-stream (0 bytes available).
// Returns io.ErrUnexpectedEOF if the stream ends mid-frame.
// Returns an error if the frame length exceeds MaxFrameSize.
func ReadFrame(r io.Reader) (msgType byte, payload []byte, err error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			// Partial length header — stream ended mid-frame.
			return 0, nil, io.ErrUnexpectedEOF
		}
		// io.ReadFull returns io.EOF only when 0 bytes were read.
		return 0, nil, err
	}

	frameLen := binary.BigEndian.Uint32(lenBuf[:])
	if frameLen == 0 {
		return 0, nil, fmt.Errorf("invalid frame: length is 0")
	}
	if frameLen > MaxFrameSize {
		return 0, nil, fmt.Errorf("frame too large: %d > %d", frameLen, MaxFrameSize)
	}

	data := make([]byte, frameLen)
	if _, err := io.ReadFull(r, data); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}

	return data[0], data[1:], nil
}

// TryParse checks whether buf contains a complete frame at the front.
// If yes, returns the type, the offset where payload starts (always 5),
// and the total frame length (4 + 1 + payload_len). ok=false if buf is
// too short for a complete frame or the length exceeds MaxFrameSize.
func TryParse(buf []byte) (msgType byte, payloadStart int, totalLen int, ok bool) {
	if len(buf) < 5 {
		// Need at least 4 (length) + 1 (type) bytes.
		return 0, 0, 0, false
	}

	frameLen := binary.BigEndian.Uint32(buf[0:4])
	if frameLen == 0 || frameLen > MaxFrameSize {
		return 0, 0, 0, false
	}

	total := 4 + int(frameLen)
	if len(buf) < total {
		return 0, 0, 0, false
	}

	return buf[4], 5, total, true
}

// SendJSON JSON-encodes v and sends it as a typed frame.
func SendJSON(w io.Writer, msgType byte, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("json marshal: %w", err)
	}
	return WriteFrame(w, msgType, payload)
}

// ResizePayload encodes terminal dimensions as [u16 rows BE][u16 cols BE].
func ResizePayload(rows, cols uint16) [4]byte {
	var buf [4]byte
	binary.BigEndian.PutUint16(buf[0:2], rows)
	binary.BigEndian.PutUint16(buf[2:4], cols)
	return buf
}

// ParseResize decodes a RESIZE payload. Returns ok=false if len < 4.
func ParseResize(payload []byte) (rows, cols uint16, ok bool) {
	if len(payload) < 4 {
		return 0, 0, false
	}
	rows = binary.BigEndian.Uint16(payload[0:2])
	cols = binary.BigEndian.Uint16(payload[2:4])
	return rows, cols, true
}

// ExitPayload encodes an exit code as [i32 BE].
func ExitPayload(code int32) [4]byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[0:4], uint32(code))
	return buf
}

// ParseExitCode decodes an EXIT payload. Returns ok=false if len < 4.
func ParseExitCode(payload []byte) (int32, bool) {
	if len(payload) < 4 {
		return 0, false
	}
	return int32(binary.BigEndian.Uint32(payload[0:4])), true
}
