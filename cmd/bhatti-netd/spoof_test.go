package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/gateway"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

func TestGuestFrameCannotBorrowAnotherPolicy(t *testing.T) {
	guestSide, netdSide := net.Pipe()
	defer guestSide.Close()
	defer netdSide.Close()
	gw, err := NewGateway(tcpip.AddrFrom4(testGwIP), 24, testGwMAC, hostProtection{})
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range []struct{ ip, mac, token string }{
		{"100.64.0.2", "52:54:00:00:00:02", testNetToken},
		{"100.64.0.3", "52:54:00:00:00:03", strings.Repeat("cd", 32)},
	} {
		if err := gw.SetSandbox(identity.ip, identity.ip, &gateway.EgressPolicy{Default: gateway.PosturePublic}, identity.mac, identity.token); err != nil {
			t.Fatal(err)
		}
	}
	gw.AddGuest(gateway.NewFrameConn(netdSide), tcpip.AddrFrom4(testGuestIP), testGuestMAC, testNetToken)
	got := make(chan struct{}, 8)
	fwd := tcp.NewForwarder(gw.stack, 0, 16, func(r *tcp.ForwarderRequest) {
		got <- struct{}{}
		r.Complete(true)
	})
	gw.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
	// This test observes ingress at the forwarder; do not run the outbound
	// pump into net.Pipe with no reader (a generated RST can block it).
	guest := gateway.NewFrameConn(guestSide)
	check := func(name string, frame []byte, want bool) {
		t.Helper()
		if err := guest.WriteFrame(frame); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		timeout := 250 * time.Millisecond
		if want {
			timeout = 3 * time.Second
		}
		select {
		case <-got:
			if !want {
				t.Errorf("%s: spoofed SYN reached the forwarder", name)
			}
		case <-time.After(timeout):
			if want {
				t.Fatalf("%s: valid SYN did not reach the forwarder", name)
			}
		}
	}
	check("assigned identity", buildTCPSyn([4]byte{8, 8, 8, 8}, 443, false), true)
	spoofIP := buildTCPSyn([4]byte{8, 8, 8, 8}, 443, false)
	copy(spoofIP[header.EthernetMinimumSize+12:], []byte{100, 64, 0, 3})
	check("sibling's source IP", spoofIP, false)
	spoofMAC := buildTCPSyn([4]byte{8, 8, 8, 8}, 443, false)
	copy(spoofMAC[6:12], []byte{0x52, 0x54, 0, 0, 0, 3})
	check("sibling's source MAC", spoofMAC, false)
	if p := gw.lookup(tcpip.LinkAddress("\x52\x54\x00\x00\x00\x03")); p != nil {
		t.Fatal("spoofed MAC learned for a different port")
	}
	gw.DelSandbox("100.64.0.2")
	check("revoked attachment", buildTCPSyn([4]byte{8, 8, 8, 8}, 443, false), false)
}

func TestGuestARPClaimsOnlyItsOwnIPAndMAC(t *testing.T) {
	guestSide, netdSide := net.Pipe()
	defer guestSide.Close()
	defer netdSide.Close()
	gw, err := NewGateway(tcpip.AddrFrom4(testGwIP), 24, testGwMAC, hostProtection{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.SetSandbox("100.64.0.2", "sb1", nil, "52:54:00:00:00:02", testNetToken); err != nil {
		t.Fatal(err)
	}
	gw.AddGuest(gateway.NewFrameConn(netdSide), tcpip.AddrFrom4(testGuestIP), testGuestMAC, testNetToken)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go gw.Run(ctx)
	guest := gateway.NewFrameConn(guestSide)
	if err := guest.WriteFrame(arpRequestFrame()); err != nil {
		t.Fatal(err)
	}
	_ = guestSide.SetReadDeadline(time.Now().Add(time.Second))
	frame, err := guest.ReadFrame()
	if err != nil {
		t.Fatalf("valid ARP: %v", err)
	}
	assertARPReply(t, frame)
	for _, variant := range []struct {
		name   string
		mutate func([]byte)
	}{
		{"sibling IP", func(f []byte) { copy(f[header.EthernetMinimumSize+14:], []byte{100, 64, 0, 3}) }},
		{"sender hardware", func(f []byte) { f[header.EthernetMinimumSize+8+5] = 3 }},
		{"Ethernet source", func(f []byte) { f[11] = 3 }},
	} {
		request := arpRequestFrame()
		variant.mutate(request)
		if err := guest.WriteFrame(request); err != nil {
			t.Fatal(err)
		}
		_ = guestSide.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		if _, err := guest.ReadFrame(); err == nil {
			t.Errorf("%s: gateway answered forged ARP", variant.name)
		}
	}
}

func TestSpoofWarningIsRateLimitedPerAttachment(t *testing.T) {
	gw, err := NewGateway(tcpip.AddrFrom4(testGwIP), 24, testGwMAC, hostProtection{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.SetSandbox("100.64.0.2", "sb1", nil, "52:54:00:00:00:02", testNetToken); err != nil {
		t.Fatal(err)
	}
	p := &guestPort{ip: tcpip.AddrFrom4(testGuestIP), mac: testGuestMAC, token: testNetToken}
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	for i := 0; i < 3; i++ {
		gw.allowIngress(p, []byte{0})
	}
	if n := strings.Count(logs.String(), "netd.spoofed_frame"); n != 1 {
		t.Fatalf("%d warnings for one attachment: %s", n, logs.String())
	}
}
