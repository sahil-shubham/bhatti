package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/sahilshubham/bhatti/pkg/engine"
)

// tunnelMockEngine implements engine.Engine with a working Tunnel()
// that connects to a local TCP server (simulating what socat does inside
// a container).
type tunnelMockEngine struct {
	mockEngine
	tunnelAddr string // address of the mock service to tunnel to
}

func (m *tunnelMockEngine) Tunnel(_ context.Context, id string, port int) (io.ReadWriteCloser, error) {
	conn, err := net.DialTimeout("tcp", m.tunnelAddr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func TestProxyManagerForwardAndStop(t *testing.T) {
	// Start a mock "service inside the container"
	mockServer, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer mockServer.Close()

	go func() {
		for {
			conn, err := mockServer.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.Write([]byte("hello from container"))
			}(conn)
		}
	}()

	eng := &tunnelMockEngine{
		mockEngine: *newMockEngine(),
		tunnelAddr: mockServer.Addr().String(),
	}
	pm := NewProxyManager(eng)

	// Forward
	entry, err := pm.Forward("sb1", "engine-id-1", 4321)
	if err != nil {
		t.Fatal(err)
	}
	if entry.HostPort == 0 {
		t.Fatal("expected non-zero host port")
	}
	if entry.SandboxID != "sb1" {
		t.Fatalf("expected sb1, got %s", entry.SandboxID)
	}

	// Connect through the forward — data should tunnel through
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", entry.HostPort), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 100)
	n, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "hello from container" {
		t.Fatalf("expected 'hello from container', got %q", got)
	}
	conn.Close()

	// Idempotent forward — same entry returned
	entry2, err := pm.Forward("sb1", "engine-id-1", 4321)
	if err != nil {
		t.Fatal(err)
	}
	if entry2.HostPort != entry.HostPort {
		t.Fatal("expected idempotent forward to return same host port")
	}

	// ActiveForwards
	fwds := pm.ActiveForwards("sb1")
	if len(fwds) != 1 {
		t.Fatalf("expected 1 forward, got %d", len(fwds))
	}

	// AllForwards
	all := pm.AllForwards()
	if len(all) != 1 {
		t.Fatalf("expected 1 total forward, got %d", len(all))
	}

	// StopAll
	pm.StopAll("sb1")
	fwds = pm.ActiveForwards("sb1")
	if len(fwds) != 0 {
		t.Fatal("expected 0 forwards after StopAll")
	}
}

func TestProxyManagerStopForward(t *testing.T) {
	eng := &tunnelMockEngine{mockEngine: *newMockEngine(), tunnelAddr: "127.0.0.1:1"}
	pm := NewProxyManager(eng)

	pm.Forward("sb1", "eid1", 3000)
	pm.Forward("sb1", "eid1", 3001)

	fwds := pm.ActiveForwards("sb1")
	if len(fwds) != 2 {
		t.Fatalf("expected 2 forwards, got %d", len(fwds))
	}

	pm.StopForward("sb1", 3000)
	fwds = pm.ActiveForwards("sb1")
	if len(fwds) != 1 {
		t.Fatalf("expected 1 forward after stopping one, got %d", len(fwds))
	}
}

func TestProxyManagerMultipleSandboxes(t *testing.T) {
	eng := &tunnelMockEngine{mockEngine: *newMockEngine(), tunnelAddr: "127.0.0.1:1"}
	pm := NewProxyManager(eng)

	pm.Forward("sb1", "eid1", 3000)
	pm.Forward("sb2", "eid2", 3000)

	all := pm.AllForwards()
	if len(all) != 2 {
		t.Fatalf("expected 2 total forwards, got %d", len(all))
	}

	pm.StopAll("sb1")
	all = pm.AllForwards()
	if len(all) != 1 {
		t.Fatalf("expected 1 forward after stopping sb1, got %d", len(all))
	}
	if all[0].SandboxID != "sb2" {
		t.Fatalf("expected sb2, got %s", all[0].SandboxID)
	}
}

func TestProxyManagerEmptyForwards(t *testing.T) {
	eng := &tunnelMockEngine{mockEngine: *newMockEngine(), tunnelAddr: "127.0.0.1:1"}
	pm := NewProxyManager(eng)

	fwds := pm.ActiveForwards("nonexistent")
	if len(fwds) != 0 {
		t.Fatalf("expected 0 forwards, got %d", len(fwds))
	}

	all := pm.AllForwards()
	if len(all) != 0 {
		t.Fatalf("expected 0 forwards, got %d", len(all))
	}

	// Should not panic
	pm.StopAll("nonexistent")
	pm.StopForward("nonexistent", 8080)
}

// Ensure tunnelMockEngine satisfies engine.Engine at compile time.
var _ engine.Engine = (*tunnelMockEngine)(nil)
