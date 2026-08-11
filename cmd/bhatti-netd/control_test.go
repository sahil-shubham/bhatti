package main

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/gateway"

	"gvisor.dev/gvisor/pkg/tcpip"
)

// TestControlChannelPerGuestPolicy exercises the daemon->netd control channel
// end to end with no VM: a pushed deny+allow policy for one guest IP is applied
// to that guest's forwarder lookup (allow the listed host, deny the rest), an
// unregistered guest keeps the public default, and DelSandbox reverts it.
func TestControlChannelPerGuestPolicy(t *testing.T) {
	gw, err := NewGateway(tcpip.AddrFrom4([4]byte{100, 64, 0, 1}), 24, testGwMAC)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	// Short UDS dir: t.TempDir() on macOS exceeds the AF_UNIX sockaddr_un limit.
	dir, err := os.MkdirTemp("/tmp", "bctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "ctl.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen control uds: %v", err)
	}
	defer ln.Close()
	go gateway.ServeControl(ln, gw)

	guest := tcpip.AddrFrom4([4]byte{100, 64, 0, 5})
	pub := netip.MustParseAddr("1.2.3.4")

	// Before any push, the guest is unregistered -> default (public) posture.
	if gw.stateFor(guest) != gw.defState {
		t.Fatal("guest should be unregistered before any push")
	}

	c := gateway.NewControlClient(sock)
	defer c.Close()
	if err := c.Send(gateway.ControlMsg{
		Op:      gateway.ControlSet,
		GuestIP: "100.64.0.5",
		Sandbox: "sb1",
		Policy:  &gateway.NetPolicyWire{Default: "deny", AllowHosts: []string{"api.allowed.test"}},
	}); err != nil {
		t.Fatalf("send set: %v", err)
	}

	// ServeControl applies asynchronously; wait for the registration to land.
	st := waitRegistered(t, gw, guest)
	if v := st.pol.Check("api.allowed.test", pub); !v.Allow {
		t.Fatalf("allow-listed host should be permitted under deny: %s", v.Reason)
	}
	if v := st.pol.Check("evil.test", pub); v.Allow {
		t.Fatal("deny default must block a non-allow-listed host")
	}

	// A different, unregistered guest still gets the public default.
	other := tcpip.AddrFrom4([4]byte{100, 64, 0, 9})
	if v := gw.stateFor(other).pol.Check("", pub); !v.Allow {
		t.Fatal("unregistered guest should default to public egress")
	}

	// DelSandbox reverts the guest to the default.
	if err := c.Send(gateway.ControlMsg{Op: gateway.ControlDel, GuestIP: "100.64.0.5"}); err != nil {
		t.Fatalf("send del: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gw.stateFor(guest) == gw.defState {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("guest still registered after DelSandbox")
}

func waitRegistered(t *testing.T, gw *Gateway, addr tcpip.Address) *guestState {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := gw.stateFor(addr); st != gw.defState {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("guest not registered after SetSandbox push")
	return nil
}
