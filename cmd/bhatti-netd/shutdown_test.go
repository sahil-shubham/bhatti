package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/gateway"
	"go.uber.org/goleak"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

func TestServeStopsWithAnOpenTCPForward(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	// Loopback is deliberately hard-denied even to host services. Use an
	// interface address, explicitly allowing just this local test server.
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var ip netip.Addr
	for _, addr := range addrs {
		p, err := netip.ParsePrefix(addr.String())
		if err == nil && p.Addr().Is4() && p.Addr().IsGlobalUnicast() {
			ip = p.Addr()
			break
		}
	}
	if !ip.IsValid() {
		t.Skip("an IPv4 interface is needed for a local upstream outside loopback")
	}
	upstream, err := net.Listen("tcp4", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	port := uint16(upstream.Addr().(*net.TCPAddr).Port)
	prefix := netip.PrefixFrom(ip, 32)
	dir := shortDir(t)
	ln, err := net.Listen("unix", filepath.Join(dir, "net.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctlLn, err := net.Listen("unix", filepath.Join(dir, "ctl.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ctlLn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- serve(ctx, ln, gwConfig{ip: tcpip.AddrFrom4(testGwIP), prefix: 24, mac: testGwMAC,
			host: hostProtection{addrs: []netip.Prefix{prefix}, services: []netip.AddrPort{netip.AddrPortFrom(ip, port)}}}, ctlLn, nil)
	}()
	ctl := gateway.NewControlClient(ctlLn.Addr().String())
	defer ctl.Close()
	sendControl(t, ctl, gateway.ControlMsg{Op: gateway.ControlSet, GuestIP: statsGuestIP, GuestMAC: statsGuestMAC,
		GuestToken: testNetToken, Sandbox: "shutdown-test", Policy: &gateway.NetPolicyWire{Default: "deny", AllowCIDRs: []string{prefix.String()}}})
	conn, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, testNetToken); err != nil {
		t.Fatal(err)
	}
	guest := gateway.NewFrameConn(conn)
	if err := guest.WriteFrame(arpRequestFrame()); err != nil {
		t.Fatal(err)
	}
	assertARPReply(t, readFrameCtx(t, guest, 5*time.Second))
	if err := guest.WriteFrame(buildTCPSyn(ip.As4(), port, false)); err != nil {
		t.Fatal(err)
	}
	if err := upstream.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	up, err := upstream.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	synack := readFrameCtx(t, guest, 5*time.Second)
	iph := header.IPv4(synack[header.EthernetMinimumSize:])
	tcph := header.TCP(iph.Payload())
	if tcph.Flags()&(header.TCPFlagSyn|header.TCPFlagAck) != header.TCPFlagSyn|header.TCPFlagAck {
		t.Fatalf("reply flags = %v, want SYN+ACK", tcph.Flags())
	}
	ack := buildTCPSyn(ip.As4(), port, false)
	ack = append(ack, []byte("ping")...)
	aip := header.IPv4(ack[header.EthernetMinimumSize:])
	aip.SetTotalLength(uint16(len(ack) - header.EthernetMinimumSize))
	aip.SetChecksum(0)
	aip.SetChecksum(^aip.CalculateChecksum())
	atcp := header.TCP(aip.Payload())
	atcp.Encode(&header.TCPFields{SrcPort: 45000, DstPort: port, SeqNum: tcph.AckNumber(), AckNum: tcph.SequenceNumber() + 1,
		DataOffset: header.TCPMinimumSize, Flags: header.TCPFlagAck | header.TCPFlagPsh, WindowSize: 65535})
	atcp.SetChecksum(^atcp.CalculateChecksum(header.PseudoHeaderChecksum(header.TCPProtocolNumber,
		tcpip.AddrFrom4(testGuestIP), tcpip.AddrFrom4(ip.As4()), uint16(len(atcp)))))
	if err := guest.WriteFrame(ack); err != nil {
		t.Fatal(err)
	}
	if err := up.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var payload [4]byte
	if _, err := io.ReadFull(up, payload[:]); err != nil || string(payload[:]) != "ping" {
		t.Fatalf("forwarded payload = %q, %v", payload, err)
	}
	// Keep both peers open: cancellation must abort the stack endpoint rather
	// than waiting for an upstream EOF or a guest-initiated close.
	start := time.Now()
	cancel()
	select {
	case err := <-served:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop with a live forward")
	}
	if _, err := up.Read(payload[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("open upstream after shutdown = %v, want EOF from the aborted guest endpoint", err)
	}
	t.Logf("gateway stopped and aborted an open TCP forward in %s", time.Since(start))
}
