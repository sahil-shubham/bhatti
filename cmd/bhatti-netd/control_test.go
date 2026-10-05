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

// TestControlChannelPerGuestPolicy proves that the daemon's policy replaces the
// deny fallback, while unregistered/deleted guests cannot egress.
func TestControlChannelPerGuestPolicy(t *testing.T) {
	gw, err := NewGateway(tcpip.AddrFrom4([4]byte{100, 64, 0, 1}), 24, testGwMAC, hostProtection{})
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

	if v := gw.stateFor(guest).pol.Check("", pub); v.Allow {
		t.Fatalf("unregistered guest admitted public egress: %+v", v)
	}

	c := gateway.NewControlClient(sock)
	defer c.Close()
	if err := c.Send(gateway.ControlMsg{
		Op:         gateway.ControlSet,
		GuestIP:    "100.64.0.5",
		GuestMAC:   "52:54:00:00:00:05",
		GuestToken: testNetToken,
		Sandbox:    "sb1",
		Policy:     &gateway.NetPolicyWire{Default: "deny", AllowHosts: []string{"api.allowed.test"}},
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

	// A different unregistered guest cannot inherit the registered policy.
	other := tcpip.AddrFrom4([4]byte{100, 64, 0, 9})
	if v := gw.stateFor(other).pol.Check("", pub); v.Allow {
		t.Fatal("unregistered guest admitted public egress")
	}

	// DelSandbox restores deny, not a public fallback.
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

func TestMalformedControlPolicyRevokesPreviousGrant(t *testing.T) {
	gw, err := NewGateway(tcpip.AddrFrom4(testGwIP), 24, testGwMAC, hostProtection{})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "bctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "ctl.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go gateway.ServeControl(ln, gw)
	c := gateway.NewControlClient(ln.Addr().String())
	defer c.Close()

	guest := tcpip.AddrFrom4(testGuestIP)
	ip := netip.MustParseAddr("1.2.3.4")
	public := gateway.NetPolicyWire{Default: "public"}
	if err := c.Send(gateway.ControlMsg{Op: gateway.ControlSet, GuestIP: "100.64.0.2", GuestMAC: "52:54:00:00:00:02", GuestToken: testNetToken, Sandbox: "sb1", Policy: &public}); err != nil {
		t.Fatal(err)
	}
	if v := waitRegistered(t, gw, guest).pol.Check("", ip); !v.Allow {
		t.Fatalf("initial public policy denied: %+v", v)
	}

	invalid := gateway.NetPolicyWire{Default: "public", AllowCIDRs: []string{"not a CIDR"}}
	if err := c.Send(gateway.ControlMsg{Op: gateway.ControlSet, GuestIP: "100.64.0.2", GuestMAC: "52:54:00:00:00:02", GuestToken: testNetToken, Sandbox: "sb1", Policy: &invalid}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := gw.stateFor(guest); st.pol.Default == gateway.PostureDeny {
			if v := st.pol.Check("", ip); v.Allow {
				t.Fatalf("bad control policy retained public egress: %+v", v)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("invalid policy did not replace previous public policy with deny")
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
