//go:build linux

package main

import "testing"

func BenchmarkRingBufferWrite4KB(b *testing.B) {
	r := newRingBuffer(65536) // 64KB, same as production
	data := make([]byte, 4096)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Write(data)
	}
}

func BenchmarkRingBufferWrite32KB(b *testing.B) {
	r := newRingBuffer(65536)
	data := make([]byte, 32768)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Write(data)
	}
}

func BenchmarkRingBufferWrite64(b *testing.B) {
	r := newRingBuffer(65536)
	data := make([]byte, 64) // typical terminal line
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Write(data)
	}
}

func BenchmarkRingBufferBytes(b *testing.B) {
	r := newRingBuffer(65536)
	// Fill the buffer first
	data := make([]byte, 65536)
	r.Write(data)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Bytes()
	}
}
