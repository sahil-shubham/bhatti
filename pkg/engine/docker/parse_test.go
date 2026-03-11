package docker

import (
	"sort"
	"testing"
)

func TestParseSSOutput(t *testing.T) {
	input := `LISTEN  0       128          0.0.0.0:8080       0.0.0.0:*
LISTEN  0       128          0.0.0.0:3000       0.0.0.0:*
LISTEN  0       128        127.0.0.1:9090       0.0.0.0:*`

	ports := parseSSOutput(input)
	sort.Ints(ports)

	if len(ports) != 3 {
		t.Fatalf("expected 3 ports, got %d: %v", len(ports), ports)
	}
	expected := []int{3000, 8080, 9090}
	for i, p := range ports {
		if p != expected[i] {
			t.Fatalf("expected %v, got %v", expected, ports)
		}
	}
}

func TestParseSSOutputEmpty(t *testing.T) {
	ports := parseSSOutput("")
	if len(ports) != 0 {
		t.Fatalf("expected 0 ports, got %d", len(ports))
	}
}

func TestParseSSOutputDuplicates(t *testing.T) {
	input := `LISTEN  0       128          0.0.0.0:8080       0.0.0.0:*
LISTEN  0       128        127.0.0.1:8080       0.0.0.0:*`

	ports := parseSSOutput(input)
	if len(ports) != 1 {
		t.Fatalf("expected 1 port (deduped), got %d: %v", len(ports), ports)
	}
}

func TestParseProcNetTCP(t *testing.T) {
	// /proc/net/tcp format
	input := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12345 1 0000000000000000 100 0 0 10 0
   1: 00000000:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12346 1 0000000000000000 100 0 0 10 0
   2: 0100007F:2382 00000000:0000 01 00000000:00000000 00:00000000 00000000     0        0 12347 1 0000000000000000 100 0 0 10 0`

	ports := parseProcNetTCP(input)
	sort.Ints(ports)

	// 0x1F90 = 8080, 0x0BB8 = 3000, 0x2382 state=01 (ESTABLISHED, not LISTEN)
	if len(ports) != 2 {
		t.Fatalf("expected 2 ports, got %d: %v", len(ports), ports)
	}
	expected := []int{3000, 8080}
	for i, p := range ports {
		if p != expected[i] {
			t.Fatalf("expected %v, got %v", expected, ports)
		}
	}
}

func TestParseProcNetTCPEmpty(t *testing.T) {
	ports := parseProcNetTCP("")
	if len(ports) != 0 {
		t.Fatalf("expected 0 ports, got %d", len(ports))
	}
}
