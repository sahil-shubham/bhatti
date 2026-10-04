package server

import (
	"bytes"
	"testing"
	"unicode/utf8"
)

// Every frame the piped relay emits must be valid UTF-8, and splitting the
// same output at any byte offset must reassemble to the original text.
func TestUTF8FramerSplitsAnywhere(t *testing.T) {
	text := []byte("héllo → 世界 🙂 done\n")
	for cut1 := 0; cut1 <= len(text); cut1++ {
		for cut2 := cut1; cut2 <= len(text); cut2++ {
			var f utf8Framer
			var got []byte
			for _, chunk := range [][]byte{text[:cut1], text[cut1:cut2], text[cut2:]} {
				frame := f.next(chunk)
				if !utf8.Valid(frame) {
					t.Fatalf("cuts %d,%d: invalid frame %q", cut1, cut2, frame)
				}
				got = append(got, frame...)
			}
			got = append(got, f.flush()...)
			if !bytes.Equal(got, text) {
				t.Fatalf("cuts %d,%d: reassembled %q, want %q", cut1, cut2, got, text)
			}
		}
	}
}

// Bytes that are not UTF-8 at all can't travel in a text frame: they become
// U+FFFD, and so does a character the stream ended in the middle of.
func TestUTF8FramerInvalidAndTruncated(t *testing.T) {
	var f utf8Framer
	if got := string(f.next([]byte("a\xffb"))); got != "a\uFFFDb" {
		t.Fatalf("invalid byte: got %q", got)
	}
	if got := f.next([]byte("x\xe4\xb8")); string(got) != "x" { // first 2 bytes of 世
		t.Fatalf("held-back prefix: got %q", got)
	}
	if got := string(f.flush()); got != "\uFFFD" {
		t.Fatalf("flush of a truncated character: got %q", got)
	}
}
