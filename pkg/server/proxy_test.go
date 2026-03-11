package server

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func TestProxyManagerForwardAndStop(t *testing.T) {
	pm := NewProxyManager()

	// Start a mock "container" server
	mockServer, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer mockServer.Close()
	mockPort := mockServer.Addr().(*net.TCPAddr).Port

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

	// Forward
	entry, err := pm.Forward("sb1", "127.0.0.1", mockPort)
	if err != nil {
		t.Fatal(err)
	}
	if entry.HostPort == 0 {
		t.Fatal("expected non-zero host port")
	}
	if entry.SandboxID != "sb1" {
		t.Fatalf("expected sb1, got %s", entry.SandboxID)
	}

	// Connect through the forward
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
	entry2, err := pm.Forward("sb1", "127.0.0.1", mockPort)
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

	// PortOwner
	owner, ok := pm.PortOwner(entry.HostPort)
	if !ok || owner != "sb1" {
		t.Fatalf("expected sb1 owns port %d", entry.HostPort)
	}

	// StopAll
	pm.StopAll("sb1")
	fwds = pm.ActiveForwards("sb1")
	if len(fwds) != 0 {
		t.Fatal("expected 0 forwards after StopAll")
	}
	_, ok = pm.PortOwner(entry.HostPort)
	if ok {
		t.Fatal("expected port to be released")
	}
}

func TestProxyManagerStopForward(t *testing.T) {
	pm := NewProxyManager()

	mockServer, _ := net.Listen("tcp", "127.0.0.1:0")
	defer mockServer.Close()
	mockPort := mockServer.Addr().(*net.TCPAddr).Port

	pm.Forward("sb1", "127.0.0.1", mockPort)
	pm.Forward("sb1", "127.0.0.1", mockPort+1) // different container port (will fail to connect but that's fine for this test)

	fwds := pm.ActiveForwards("sb1")
	if len(fwds) != 2 {
		t.Fatalf("expected 2 forwards, got %d", len(fwds))
	}

	pm.StopForward("sb1", mockPort)
	fwds = pm.ActiveForwards("sb1")
	if len(fwds) != 1 {
		t.Fatalf("expected 1 forward after stopping one, got %d", len(fwds))
	}
}

func TestProxyManagerMultipleSandboxes(t *testing.T) {
	pm := NewProxyManager()

	mockServer, _ := net.Listen("tcp", "127.0.0.1:0")
	defer mockServer.Close()
	mockPort := mockServer.Addr().(*net.TCPAddr).Port

	pm.Forward("sb1", "127.0.0.1", mockPort)
	pm.Forward("sb2", "127.0.0.1", mockPort)

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
	pm := NewProxyManager()

	fwds := pm.ActiveForwards("nonexistent")
	if len(fwds) != 0 {
		t.Fatalf("expected 0 forwards, got %d", len(fwds))
	}

	all := pm.AllForwards()
	if len(all) != 0 {
		t.Fatalf("expected 0 forwards, got %d", len(all))
	}

	// StopAll on nonexistent sandbox should not panic
	pm.StopAll("nonexistent")
	pm.StopForward("nonexistent", 8080)
}
