package proto

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
)

func TestWriteReadRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		msgType byte
		payload []byte
	}{
		{"empty payload", STDOUT, nil},
		{"single byte", STDERR, []byte{0x42}},
		{"large payload 64KB", STDOUT, make([]byte, 64*1024)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteFrame(&buf, tt.msgType, tt.payload); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}

			gotType, gotPayload, err := ReadFrame(&buf)
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}

			if gotType != tt.msgType {
				t.Errorf("type: got 0x%02x, want 0x%02x", gotType, tt.msgType)
			}
			if !bytes.Equal(gotPayload, tt.payload) {
				t.Errorf("payload length: got %d, want %d", len(gotPayload), len(tt.payload))
			}
		})
	}
}

func TestReadFrameEOF(t *testing.T) {
	r := bytes.NewReader(nil)
	_, _, err := ReadFrame(r)
	if err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

func TestReadFrameUnexpectedEOF(t *testing.T) {
	// Write only a partial length header (3 bytes instead of 4).
	r := bytes.NewReader([]byte{0x00, 0x00, 0x05})
	_, _, err := ReadFrame(r)
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("expected io.ErrUnexpectedEOF, got %v", err)
	}
}

func TestReadFrameUnexpectedEOFMidPayload(t *testing.T) {
	// Valid length header claiming 10 bytes, but only 3 bytes of data follow.
	var buf bytes.Buffer
	buf.Write([]byte{0x00, 0x00, 0x00, 0x0A}) // length = 10
	buf.Write([]byte{STDOUT, 0x01, 0x02})      // only 3 bytes (need 10)
	_, _, err := ReadFrame(&buf)
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("expected io.ErrUnexpectedEOF, got %v", err)
	}
}

func TestMaxFrameSize(t *testing.T) {
	// WriteFrame with payload > 1MB should fail.
	payload := make([]byte, MaxFrameSize) // 1MB payload + 1 byte type = MaxFrameSize+1
	var buf bytes.Buffer
	err := WriteFrame(&buf, STDOUT, payload)
	if err == nil {
		t.Fatal("expected error for oversized frame, got nil")
	}

	// ReadFrame with a length header claiming 2MB should fail.
	buf.Reset()
	buf.Write([]byte{0x00, 0x20, 0x00, 0x00}) // length = 2MB
	buf.Write([]byte{STDOUT})
	_, _, err = ReadFrame(&buf)
	if err == nil {
		t.Fatal("expected error for oversized frame in ReadFrame, got nil")
	}
}

func TestTryParsePartial(t *testing.T) {
	// Too short: only 3 bytes.
	_, _, _, ok := TryParse([]byte{0x00, 0x00, 0x05})
	if ok {
		t.Fatal("expected ok=false for 3-byte buffer")
	}

	// Complete frame: length=2 (1 type + 1 payload), then type + 1 byte payload.
	frame := []byte{0x00, 0x00, 0x00, 0x02, STDOUT, 0x42}
	msgType, payloadStart, totalLen, ok := TryParse(frame)
	if !ok {
		t.Fatal("expected ok=true for complete frame")
	}
	if msgType != STDOUT {
		t.Errorf("type: got 0x%02x, want 0x%02x", msgType, STDOUT)
	}
	if payloadStart != 5 {
		t.Errorf("payloadStart: got %d, want 5", payloadStart)
	}
	if totalLen != 6 {
		t.Errorf("totalLen: got %d, want 6", totalLen)
	}

	// Frame plus extra trailing bytes: totalLen should not include extras.
	withExtra := append(frame, 0xFF, 0xFF, 0xFF)
	_, _, totalLen, ok = TryParse(withExtra)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if totalLen != 6 {
		t.Errorf("totalLen with extras: got %d, want 6", totalLen)
	}

	// Incomplete frame: header says length=10 but only 6 bytes total.
	incomplete := []byte{0x00, 0x00, 0x00, 0x0A, STDOUT, 0x42}
	_, _, _, ok = TryParse(incomplete)
	if ok {
		t.Fatal("expected ok=false for incomplete frame")
	}
}

func TestSendJSON(t *testing.T) {
	req := ExecRequest{
		Argv: []string{"echo", "hello"},
		Env:  map[string]string{"FOO": "bar"},
	}

	var buf bytes.Buffer
	if err := SendJSON(&buf, EXEC_REQ, req); err != nil {
		t.Fatalf("SendJSON: %v", err)
	}

	msgType, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if msgType != EXEC_REQ {
		t.Errorf("type: got 0x%02x, want 0x%02x", msgType, EXEC_REQ)
	}

	var got ExecRequest
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(got.Argv) != 2 || got.Argv[0] != "echo" || got.Argv[1] != "hello" {
		t.Errorf("Argv: got %v", got.Argv)
	}
	if got.Env["FOO"] != "bar" {
		t.Errorf("Env[FOO]: got %q", got.Env["FOO"])
	}
}

func TestResizePayload(t *testing.T) {
	tests := []struct {
		rows, cols uint16
	}{
		{24, 80},
		{0, 0},
		{0xFFFF, 0xFFFF},
	}

	for _, tt := range tests {
		buf := ResizePayload(tt.rows, tt.cols)
		gotRows, gotCols, ok := ParseResize(buf[:])
		if !ok {
			t.Fatalf("ParseResize returned ok=false for rows=%d cols=%d", tt.rows, tt.cols)
		}
		if gotRows != tt.rows || gotCols != tt.cols {
			t.Errorf("got rows=%d cols=%d, want rows=%d cols=%d", gotRows, gotCols, tt.rows, tt.cols)
		}
	}

	// Too short payload.
	_, _, ok := ParseResize([]byte{0x00, 0x01})
	if ok {
		t.Fatal("expected ok=false for short payload")
	}
}

func TestExitPayload(t *testing.T) {
	codes := []int32{0, 1, -1, 127, 137, 143}

	for _, code := range codes {
		buf := ExitPayload(code)
		got, ok := ParseExitCode(buf[:])
		if !ok {
			t.Fatalf("ParseExitCode returned ok=false for code=%d", code)
		}
		if got != code {
			t.Errorf("got %d, want %d", got, code)
		}
	}

	// Too short payload.
	_, ok := ParseExitCode([]byte{0x00})
	if ok {
		t.Fatal("expected ok=false for short payload")
	}
}

func TestConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	var wg sync.WaitGroup

	const perGoroutine = 1000
	const goroutines = 2

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		msgType := byte(STDOUT + byte(g)) // STDOUT=0x02, STDERR=0x03
		go func(mt byte) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				payload := []byte{mt, byte(i & 0xFF)}
				mu.Lock()
				if err := WriteFrame(&buf, mt, payload); err != nil {
					mu.Unlock()
					t.Errorf("WriteFrame: %v", err)
					return
				}
				mu.Unlock()
			}
		}(msgType)
	}

	wg.Wait()

	// Read all frames back. None should be corrupt.
	total := goroutines * perGoroutine
	for i := 0; i < total; i++ {
		msgType, payload, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame %d/%d: %v", i+1, total, err)
		}
		if msgType != STDOUT && msgType != STDERR {
			t.Fatalf("frame %d: unexpected type 0x%02x", i, msgType)
		}
		if len(payload) != 2 {
			t.Fatalf("frame %d: payload len %d, want 2", i, len(payload))
		}
		if payload[0] != msgType {
			t.Errorf("frame %d: payload[0]=0x%02x, want 0x%02x", i, payload[0], msgType)
		}
	}

	// Buffer should be fully consumed.
	if buf.Len() != 0 {
		t.Errorf("buffer has %d bytes remaining", buf.Len())
	}
}

// TestNetPipeRoundTrip verifies framing works over a real bidirectional
// connection (net.Pipe), not just bytes.Buffer. This catches issues with
// partial reads, buffering, and concurrent bidirectional I/O.
func TestNetPipeRoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	// Writer goroutine: send frames from c1.
	errc := make(chan error, 1)
	go func() {
		if err := WriteFrame(c1, EXEC_REQ, []byte(`{"argv":["ls"]}`)); err != nil {
			errc <- err
			return
		}
		if err := WriteFrame(c1, STDIN, []byte("hello")); err != nil {
			errc <- err
			return
		}
		payload := ExitPayload(42)
		if err := WriteFrame(c1, EXIT, payload[:]); err != nil {
			errc <- err
			return
		}
		errc <- nil
	}()

	// Reader: read frames from c2.
	msgType, payload, err := ReadFrame(c2)
	if err != nil {
		t.Fatalf("frame 1: %v", err)
	}
	if msgType != EXEC_REQ || string(payload) != `{"argv":["ls"]}` {
		t.Errorf("frame 1: type=0x%02x payload=%q", msgType, payload)
	}

	msgType, payload, err = ReadFrame(c2)
	if err != nil {
		t.Fatalf("frame 2: %v", err)
	}
	if msgType != STDIN || string(payload) != "hello" {
		t.Errorf("frame 2: type=0x%02x payload=%q", msgType, payload)
	}

	msgType, payload, err = ReadFrame(c2)
	if err != nil {
		t.Fatalf("frame 3: %v", err)
	}
	if msgType != EXIT {
		t.Errorf("frame 3: type=0x%02x, want EXIT", msgType)
	}
	code, ok := ParseExitCode(payload)
	if !ok || code != 42 {
		t.Errorf("frame 3: exit code=%d ok=%v", code, ok)
	}

	if err := <-errc; err != nil {
		t.Fatalf("writer: %v", err)
	}
}

// TestNetPipeBidirectional verifies both sides can write/read concurrently,
// simulating the real host↔guest protocol exchange.
func TestNetPipeBidirectional(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	// Simulate host→guest: EXEC_REQ then STDIN frames.
	// Simulate guest→host: STDOUT then EXIT frames.
	errc := make(chan error, 2)

	// "Host" side on c1: write request, read response.
	go func() {
		if err := WriteFrame(c1, EXEC_REQ, []byte(`{"argv":["echo","hi"]}`)); err != nil {
			errc <- err
			return
		}
		// Read STDOUT
		mt, p, err := ReadFrame(c1)
		if err != nil {
			errc <- err
			return
		}
		if mt != STDOUT || string(p) != "hi\n" {
			errc <- io.ErrUnexpectedEOF
			return
		}
		// Read EXIT
		mt, p, err = ReadFrame(c1)
		if err != nil {
			errc <- err
			return
		}
		if mt != EXIT {
			errc <- io.ErrUnexpectedEOF
			return
		}
		errc <- nil
	}()

	// "Guest" side on c2: read request, write response.
	go func() {
		mt, p, err := ReadFrame(c2)
		if err != nil {
			errc <- err
			return
		}
		if mt != EXEC_REQ {
			errc <- io.ErrUnexpectedEOF
			return
		}
		_ = p
		if err := WriteFrame(c2, STDOUT, []byte("hi\n")); err != nil {
			errc <- err
			return
		}
		exit := ExitPayload(0)
		if err := WriteFrame(c2, EXIT, exit[:]); err != nil {
			errc <- err
			return
		}
		errc <- nil
	}()

	for i := 0; i < 2; i++ {
		if err := <-errc; err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
}

// TestMultipleSequentialFrames writes many frames to a single stream
// and reads them all back, verifying order and content.
func TestMultipleSequentialFrames(t *testing.T) {
	var buf bytes.Buffer
	types := []byte{STDIN, STDOUT, STDERR, RESIZE, EXIT, ERROR, KILL, EXEC_REQ, FWD_REQ, FWD_RESP}

	for i, mt := range types {
		payload := []byte{byte(i), byte(i + 1)}
		if err := WriteFrame(&buf, mt, payload); err != nil {
			t.Fatalf("WriteFrame %d: %v", i, err)
		}
	}

	for i, wantType := range types {
		gotType, gotPayload, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame %d: %v", i, err)
		}
		if gotType != wantType {
			t.Errorf("frame %d: type 0x%02x, want 0x%02x", i, gotType, wantType)
		}
		if len(gotPayload) != 2 || gotPayload[0] != byte(i) || gotPayload[1] != byte(i+1) {
			t.Errorf("frame %d: payload %v, want [%d %d]", i, gotPayload, i, i+1)
		}
	}

	// Stream should be fully consumed.
	_, _, err := ReadFrame(&buf)
	if err != io.EOF {
		t.Errorf("expected io.EOF after all frames, got %v", err)
	}
}

// TestSendJSONForwardRequest tests ForwardRequest JSON round-trip.
func TestSendJSONForwardRequest(t *testing.T) {
	req := ForwardRequest{Port: 8080}
	var buf bytes.Buffer
	if err := SendJSON(&buf, FWD_REQ, req); err != nil {
		t.Fatalf("SendJSON: %v", err)
	}

	msgType, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if msgType != FWD_REQ {
		t.Errorf("type: 0x%02x, want 0x%02x", msgType, FWD_REQ)
	}

	var got ForwardRequest
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Port != 8080 {
		t.Errorf("Port: %d, want 8080", got.Port)
	}
}

// TestSendJSONForwardResponse tests ForwardResponse JSON round-trip,
// including both "ok" and "error" cases.
func TestSendJSONForwardResponse(t *testing.T) {
	// Success response.
	resp := ForwardResponse{Status: "ok"}
	var buf bytes.Buffer
	if err := SendJSON(&buf, FWD_RESP, resp); err != nil {
		t.Fatalf("SendJSON ok: %v", err)
	}

	msgType, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if msgType != FWD_RESP {
		t.Errorf("type: 0x%02x, want 0x%02x", msgType, FWD_RESP)
	}
	var got ForwardResponse
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Status != "ok" {
		t.Errorf("Status: %q, want %q", got.Status, "ok")
	}
	if got.Message != nil {
		t.Errorf("Message should be nil for ok response, got %q", *got.Message)
	}

	// Error response.
	errMsg := "connection refused"
	resp = ForwardResponse{Status: "error", Message: &errMsg}
	buf.Reset()
	if err := SendJSON(&buf, FWD_RESP, resp); err != nil {
		t.Fatalf("SendJSON error: %v", err)
	}

	_, payload, err = ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	var gotErr ForwardResponse
	if err := json.Unmarshal(payload, &gotErr); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if gotErr.Status != "error" {
		t.Errorf("Status: %q, want %q", gotErr.Status, "error")
	}
	if gotErr.Message == nil || *gotErr.Message != "connection refused" {
		t.Errorf("Message: %v, want %q", gotErr.Message, "connection refused")
	}
}

// TestExecRequestOptionalFields tests JSON round-trip of ExecRequest with
// all optional pointer fields set, and with them nil (omitted).
func TestExecRequestOptionalFields(t *testing.T) {
	// All fields set.
	ttyTrue := true
	rows := uint16(40)
	cols := uint16(120)
	cwd := "/workspace"
	req := ExecRequest{
		Argv: []string{"/bin/zsh", "-li"},
		Env:  map[string]string{"TERM": "xterm-256color"},
		TTY:  &ttyTrue,
		Rows: &rows,
		Cols: &cols,
		Cwd:  &cwd,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got ExecRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.TTY == nil || !*got.TTY {
		t.Error("TTY should be true")
	}
	if got.Rows == nil || *got.Rows != 40 {
		t.Errorf("Rows: %v, want 40", got.Rows)
	}
	if got.Cols == nil || *got.Cols != 120 {
		t.Errorf("Cols: %v, want 120", got.Cols)
	}
	if got.Cwd == nil || *got.Cwd != "/workspace" {
		t.Errorf("Cwd: %v, want /workspace", got.Cwd)
	}

	// Verify omitempty: nil fields should not appear in JSON.
	minimal := ExecRequest{Argv: []string{"ls"}}
	data, _ = json.Marshal(minimal)
	s := string(data)
	for _, key := range []string{"tty", "rows", "cols", "cwd", "env"} {
		if bytes.Contains(data, []byte(`"`+key+`"`)) {
			t.Errorf("nil field %q should be omitted from JSON: %s", key, s)
		}
	}
}

// TestTryParseZeroLength verifies that a frame with length=0 is rejected.
func TestTryParseZeroLength(t *testing.T) {
	// Length field = 0.
	buf := []byte{0x00, 0x00, 0x00, 0x00, STDOUT}
	_, _, _, ok := TryParse(buf)
	if ok {
		t.Fatal("expected ok=false for zero-length frame")
	}
}

// TestTryParseOversizedLength verifies that TryParse rejects oversized frames.
func TestTryParseOversizedLength(t *testing.T) {
	// Length = MaxFrameSize + 1.
	buf := make([]byte, 5)
	oversize := uint32(MaxFrameSize + 1)
	buf[0] = byte(oversize >> 24)
	buf[1] = byte(oversize >> 16)
	buf[2] = byte(oversize >> 8)
	buf[3] = byte(oversize)
	buf[4] = STDOUT
	_, _, _, ok := TryParse(buf)
	if ok {
		t.Fatal("expected ok=false for oversized frame")
	}
}

// TestWriteFrameNilVsEmpty verifies nil and empty payload produce identical wire bytes.
func TestWriteFrameNilVsEmpty(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	if err := WriteFrame(&buf1, STDOUT, nil); err != nil {
		t.Fatalf("WriteFrame nil: %v", err)
	}
	if err := WriteFrame(&buf2, STDOUT, []byte{}); err != nil {
		t.Fatalf("WriteFrame empty: %v", err)
	}
	if !bytes.Equal(buf1.Bytes(), buf2.Bytes()) {
		t.Errorf("nil and empty payload produce different wire bytes:\n  nil:   %x\n  empty: %x", buf1.Bytes(), buf2.Bytes())
	}
}

// TestReadFrameZeroLengthField verifies ReadFrame returns an error (not a hang)
// when the length field is 0.
func TestReadFrameZeroLengthField(t *testing.T) {
	buf := bytes.NewReader([]byte{0x00, 0x00, 0x00, 0x00})
	_, _, err := ReadFrame(buf)
	if err == nil {
		t.Fatal("expected error for zero-length frame, got nil")
	}
}

// TestAllFrameTypes verifies every defined frame type constant round-trips.
func TestAllFrameTypes(t *testing.T) {
	allTypes := []struct {
		name    string
		msgType byte
	}{
		{"STDIN", STDIN},
		{"STDOUT", STDOUT},
		{"STDERR", STDERR},
		{"RESIZE", RESIZE},
		{"EXIT", EXIT},
		{"ERROR", ERROR},
		{"KILL", KILL},
		{"EXEC_REQ", EXEC_REQ},
		{"FWD_REQ", FWD_REQ},
		{"FWD_RESP", FWD_RESP},
	}

	for _, tt := range allTypes {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			payload := []byte("test-" + tt.name)
			if err := WriteFrame(&buf, tt.msgType, payload); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
			gotType, gotPayload, err := ReadFrame(&buf)
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			if gotType != tt.msgType {
				t.Errorf("type: 0x%02x, want 0x%02x", gotType, tt.msgType)
			}
			if string(gotPayload) != string(payload) {
				t.Errorf("payload: %q, want %q", gotPayload, payload)
			}
		})
	}
}

func TestWriteFrameExactMaxSize(t *testing.T) {
	// Payload of MaxFrameSize-1 bytes should succeed (frame length = 1 + (MaxFrameSize-1) = MaxFrameSize).
	payload := make([]byte, MaxFrameSize-1)
	var buf bytes.Buffer
	if err := WriteFrame(&buf, STDOUT, payload); err != nil {
		t.Fatalf("WriteFrame with max-size payload should succeed: %v", err)
	}

	msgType, gotPayload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if msgType != STDOUT {
		t.Errorf("type: got 0x%02x, want 0x%02x", msgType, STDOUT)
	}
	if len(gotPayload) != MaxFrameSize-1 {
		t.Errorf("payload len: got %d, want %d", len(gotPayload), MaxFrameSize-1)
	}
}
