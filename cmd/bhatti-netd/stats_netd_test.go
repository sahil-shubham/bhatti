package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/gateway"

	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// TestNetdReportsTrafficOverItsControlSocket runs the real netd binary, as the
// daemon does: a guest registered over the control socket, its link attached to
// the net socket with the secret as the hello, and the report read back over
// the control socket the way the collector will.
func TestNetdReportsTrafficOverItsControlSocket(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build netd")
	}
	bin := filepath.Join(t.TempDir(), "bhatti-netd")
	// Built the way `make netd` builds it; on Linux confine needs a cgo-free binary.
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build bhatti-netd: %v\n%s", err, out)
	}

	dir := shortDir(t)
	netSock, ctlSock := filepath.Join(dir, "net.sock"), filepath.Join(dir, "ctl.sock")
	var stderr syncBuffer
	cmd := exec.Command(bin, "--net-uds", netSock, "--ctl-uds", ctlSock)
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start netd: %v", err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	t.Cleanup(func() { cmd.Process.Kill(); <-exited })

	// netd opens the net socket before the control socket, so once the control
	// socket answers, both are ready.
	waitFor(t, "netd to answer on its control socket", func() bool {
		select {
		case <-exited:
			t.Fatalf("netd exited early; stderr: %s", stderr.String())
		default:
		}
		return gateway.ProbeVersion(ctlSock) == nil
	})

	t.Run("no guests", func(t *testing.T) {
		if got := queryNetStats(t, ctlSock).Guests; got == nil || len(got) != 0 {
			t.Fatalf("a netd with no guests reports %#v, want an empty list", got)
		}
	})

	t.Run("a guest's traffic and refusals", func(t *testing.T) {
		ctl := gateway.NewControlClient(ctlSock)
		defer ctl.Close()
		sendControl(t, ctl, gateway.ControlMsg{Op: gateway.ControlSet, GuestIP: statsGuestIP, GuestMAC: statsGuestMAC,
			GuestToken: testNetToken, Sandbox: "sb-real", Policy: &gateway.NetPolicyWire{Default: "deny"}})
		conn, err := net.Dial("unix", netSock)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte(testNetToken)); err != nil {
			t.Fatal(err)
		}
		link := newGuestLink(conn)

		arp := arpRequestFrame()
		spoofed := synFrame(&[4]byte{100, 64, 0, 3}, [4]byte{8, 8, 8, 8}, 443)
		metadata := synFrame(nil, [4]byte{169, 254, 169, 254}, 80)
		refused := synFrame(nil, [4]byte{8, 8, 8, 8}, 443)
		for _, f := range [][]byte{arp, spoofed, metadata, refused} {
			link.send(t, f)
		}
		want := tally{txPackets: 3, txBytes: uint64(len(arp) + len(metadata) + len(refused)), spoof: 1, hardDeny: 1, policy: 1}
		waitGuest(t, ctlSock, statsGuestIP, "the counters to read "+want.String(), want.matches)

		got := waitGuest(t, ctlSock, statsGuestIP, "Rx to equal the frames the guest received", link.received)
		if got.RxBytes < header.EthernetMinimumSize+header.ARPSize {
			t.Errorf("Rx = %d bytes; the ARP reply alone is %d", got.RxBytes, header.EthernetMinimumSize+header.ARPSize)
		}
		if got.SandboxID != "sb-real" || got.GuestIP != statsGuestIP {
			t.Errorf("report names the guest %q/%q, want sb-real/%s", got.SandboxID, got.GuestIP, statsGuestIP)
		}
		if !want.matches(got) {
			t.Errorf("counters moved after the traffic settled: %+v, want %s", got, want)
		}
		t.Logf("real netd STATS: %+v", got)
	})

	t.Run("a netd that has died", func(t *testing.T) {
		cmd.Process.Kill()
		<-exited
		start := time.Now()
		_, err := gateway.QueryStats(context.Background(), ctlSock) // its socket file is still there
		if err == nil || errors.Is(err, gateway.ErrStatsUnsupported) {
			t.Fatalf("QueryStats of a dead netd = %v, want a connection error", err)
		}
		// Refused at once, not after the 1s cap.
		if d := time.Since(start); d > 900*time.Millisecond {
			t.Fatalf("took %v to fail", d)
		}
	})
}
