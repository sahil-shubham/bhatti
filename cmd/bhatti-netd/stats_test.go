package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/gateway"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

const (
	statsGuestIP  = "100.64.0.2"
	statsGuestMAC = "52:54:00:00:00:02"
)

// guestLink is the guest's end of its NIC: it sends frames and records the
// ones netd delivers, which is the ground truth for what netd calls Rx.
type guestLink struct {
	fc     *gateway.FrameConn
	frames atomic.Uint64
	bytes  atomic.Uint64
}

func newGuestLink(rw io.ReadWriter) *guestLink {
	l := &guestLink{fc: gateway.NewFrameConn(rw)}
	go func() {
		for {
			f, err := l.fc.ReadFrame()
			if err != nil {
				return
			}
			l.bytes.Add(uint64(len(f)))
			l.frames.Add(1)
		}
	}()
	return l
}

func (l *guestLink) send(t *testing.T, frame []byte) {
	t.Helper()
	if err := l.fc.WriteFrame(frame); err != nil {
		t.Fatalf("guest send: %v", err)
	}
}

// received reports whether g's Rx is what the guest actually got, and nonzero.
func (l *guestLink) received(g gateway.GuestStats) bool {
	return g.RxPackets > 0 && g.RxPackets == l.frames.Load() && g.RxBytes == l.bytes.Load()
}

// tally is what a guest's Tx and refusal counters should read.
type tally struct{ txPackets, txBytes, policy, spoof, hardDeny uint64 }

func (w tally) matches(g gateway.GuestStats) bool {
	return g.TxPackets == w.txPackets && g.TxBytes == w.txBytes &&
		g.Dropped.Policy == w.policy && g.Dropped.Spoof == w.spoof && g.Dropped.HardDeny == w.hardDeny
}

func (w tally) String() string {
	return fmt.Sprintf("tx %d frames/%d bytes, dropped policy %d spoof %d hard_deny %d",
		w.txPackets, w.txBytes, w.policy, w.spoof, w.hardDeny)
}

func shortDir(t *testing.T) string {
	t.Helper()
	// t.TempDir() on macOS is too long for an AF_UNIX path.
	dir, err := os.MkdirTemp("/tmp", "bstat")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// syncBuffer is a log sink that netd's goroutines write while a test reads it.
type syncBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func queryNetStats(t *testing.T, sock string) gateway.NetStats {
	t.Helper()
	s, err := gateway.QueryStats(context.Background(), sock)
	if err != nil {
		t.Fatalf("QueryStats: %v", err)
	}
	return s
}

func findGuest(report gateway.NetStats, ip string) (gateway.GuestStats, bool) {
	for _, g := range report.Guests {
		if g.GuestIP == ip {
			return g, true
		}
	}
	return gateway.GuestStats{}, false
}

// waitGuest polls the report read over the control socket until ok holds for
// the guest at ip. The counts the frame path and the forwarders add land
// asynchronously, so a test names the state it expects and waits for it
// instead of sleeping.
func waitGuest(t *testing.T, sock, ip, what string, ok func(gateway.GuestStats) bool) gateway.GuestStats {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		report := queryNetStats(t, sock)
		if g, found := findGuest(report, ip); found && ok(g) {
			return g
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the report never got there; last report: %+v", what, report)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// statsRig is an in-process netd served on a real control socket.
type statsRig struct {
	t    *testing.T
	gw   *Gateway
	sock string
	ctl  *gateway.ControlClient
}

func newStatsRig(t *testing.T, host hostProtection) *statsRig {
	t.Helper()
	gw := newTestGateway(t, host)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go gw.Run(ctx)
	ln, err := net.Listen("unix", filepath.Join(shortDir(t), "ctl.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go gateway.ServeControl(ln, gw)
	ctl := gateway.NewControlClient(ln.Addr().String())
	t.Cleanup(func() { ctl.Close() })
	return &statsRig{t: t, gw: gw, sock: ln.Addr().String(), ctl: ctl}
}

func sendControl(t *testing.T, ctl *gateway.ControlClient, m gateway.ControlMsg) {
	t.Helper()
	if err := ctl.Send(m); err != nil {
		t.Fatalf("control %s %s: %v", m.Op, m.GuestIP, err)
	}
}

func (r *statsRig) set(ip, sandbox, mac, token string, policy gateway.NetPolicyWire) {
	r.t.Helper()
	sendControl(r.t, r.ctl, gateway.ControlMsg{Op: gateway.ControlSet, GuestIP: ip, GuestMAC: mac,
		GuestToken: token, Sandbox: sandbox, Policy: &policy})
}

func (r *statsRig) del(ip string) {
	r.t.Helper()
	sendControl(r.t, r.ctl, gateway.ControlMsg{Op: gateway.ControlDel, GuestIP: ip})
}

// attach connects the guest at 100.64.0.2 over an in-memory link.
func (r *statsRig) attach() *guestLink {
	r.t.Helper()
	guestSide, netdSide := net.Pipe()
	r.t.Cleanup(func() { guestSide.Close(); netdSide.Close() })
	r.gw.AddGuest(gateway.NewFrameConn(netdSide), tcpip.AddrFrom4(testGuestIP), testGuestMAC, testNetToken)
	return newGuestLink(guestSide)
}

func (r *statsRig) guests() []gateway.GuestStats { return queryNetStats(r.t, r.sock).Guests }

// settle waits for the guest's counters in the gateway itself, which costs no
// socket round trip while the stack is busy (on a machine that may be saturated
// that matters), and returns them.
func (r *statsRig) settle(what string, ok func(gateway.GuestStats) bool) gateway.GuestStats {
	r.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		local := r.gw.Stats()
		if g, found := findGuest(local, statsGuestIP); found && ok(g) {
			return g
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%s: the gateway's counters never got there; last: %+v", what, local)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// expect settles, then reads the guest the way the daemon does, over the
// control socket, and requires that report to show the same state.
func (r *statsRig) expect(what string, ok func(gateway.GuestStats) bool) gateway.GuestStats {
	r.t.Helper()
	r.settle(what, ok)
	report := queryNetStats(r.t, r.sock)
	g, found := findGuest(report, statsGuestIP)
	if !found || !ok(g) {
		r.t.Fatalf("%s: the gateway had settled, but the control socket reports %+v", what, report)
	}
	return g
}

// synFrame is a TCP SYN from the guest. A non-nil claimed replaces the source
// address it carries.
func synFrame(claimed *[4]byte, dst [4]byte, dport uint16) []byte {
	f := buildTCPSyn(dst, dport, false)
	if claimed != nil {
		copy(f[header.EthernetMinimumSize+12:], claimed[:])
	}
	return f
}

func udpFrame(dst [4]byte, dport uint16, payload []byte) []byte {
	return udpGuestFrame(testGuestIP, dst, testGuestMAC, 45002, dport, payload)
}

// Every number a guest's report carries, in one place: what it sent that passed
// the identity check, what it sent that did not, and each way a connection
// attempt was turned away. Rx is checked against the frames the guest received.
func TestStatsCountTrafficAndRefusals(t *testing.T) {
	r := newStatsRig(t, hostProtection{addrs: []netip.Prefix{netip.MustParsePrefix("203.0.113.42/32")}})
	r.set(statsGuestIP, "sb-a", statsGuestMAC, testNetToken,
		gateway.NetPolicyWire{Default: "deny", AllowHosts: []string{"good.example"}})
	link := r.attach()

	sibling, stranger := [4]byte{100, 64, 0, 3}, [4]byte{100, 64, 0, 77}
	public, metadata := [4]byte{8, 8, 8, 8}, [4]byte{169, 254, 169, 254}
	steps := []struct {
		what   string
		frame  []byte
		passes bool // clears the identity check: counts as the guest's Tx
		drop   func(*tally)
	}{
		{"an ARP request for the gateway", arpRequestFrame(), true, nil},

		{"a frame claiming a sibling's address", synFrame(&sibling, public, 443), false, func(w *tally) { w.spoof++ }},
		{"a frame claiming an unregistered address", synFrame(&stranger, public, 443), false, func(w *tally) { w.spoof++ }},
		{"a runt frame", []byte{0}, false, func(w *tally) { w.spoof++ }},

		{"a connect to cloud metadata", synFrame(nil, metadata, 80), true, func(w *tally) { w.hardDeny++ }},
		{"a connect to the host's own address", synFrame(nil, [4]byte{203, 0, 113, 42}, 443), true, func(w *tally) { w.hardDeny++ }},
		{"a connect to the gateway itself", synFrame(nil, testGwIP, 80), true, func(w *tally) { w.hardDeny++ }},
		{"a connect to another owner's range", synFrame(nil, [4]byte{100, 64, 1, 5}, 80), true, func(w *tally) { w.hardDeny++ }},
		{"a datagram to cloud metadata", udpFrame(metadata, 8001, []byte("x")), true, func(w *tally) { w.hardDeny++ }},
		{"a DNS query to cloud metadata as resolver", udpFrame(metadata, 53, []byte("x")), true, func(w *tally) { w.hardDeny++ }},

		{"a connect to a public address under deny", synFrame(nil, public, 443), true, func(w *tally) { w.policy++ }},
		{"a connect to a sibling without opt-in", synFrame(nil, sibling, 80), true, func(w *tally) { w.policy++ }},
		{"a datagram to a public address under deny", udpFrame(public, 8001, []byte("x")), true, func(w *tally) { w.policy++ }},
		{"a DNS query to a private resolver", udpFrame([4]byte{10, 0, 0, 53}, 53, []byte("x")), true, func(w *tally) { w.policy++ }},
		{"a DNS query for a name outside the allow list", udpFrame(public, 53, query(t, "evil.example.")), true, func(w *tally) { w.policy++ }},
	}
	var want tally
	for _, s := range steps {
		link.send(t, s.frame)
		if s.passes {
			want.txPackets++
			want.txBytes += uint64(len(s.frame))
		}
		if s.drop != nil {
			s.drop(&want)
		}
		r.settle(s.what+" ("+want.String()+")", want.matches)
	}

	got := r.expect("the counters to read "+want.String()+" and Rx to equal the frames the guest received",
		func(g gateway.GuestStats) bool { return want.matches(g) && link.received(g) })
	if got.RxBytes < header.EthernetMinimumSize+header.ARPSize {
		t.Errorf("Rx = %d bytes; the ARP reply alone is %d", got.RxBytes, header.EthernetMinimumSize+header.ARPSize)
	}
	if got.SandboxID != "sb-a" || got.GuestIP != statsGuestIP {
		t.Errorf("report names the guest %q/%q, want sb-a/%s", got.SandboxID, got.GuestIP, statsGuestIP)
	}
	// A spoofed address is not a guest: it got no counters of its own.
	if n := len(r.guests()); n != 1 {
		t.Errorf("report lists %d guests after frames claiming other addresses, want the one registered", n)
	}
}

// The daemon re-pushes a running guest's policy, which must not reset its
// counters. Deleting the guest must, and so must handing its address to another
// sandbox, whose traffic is not the previous one's.
func TestStatsFollowTheRegistration(t *testing.T) {
	var logs syncBuffer
	quiet := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(quiet)

	r := newStatsRig(t, hostProtection{})
	deny := gateway.NetPolicyWire{Default: "deny"}
	r.set(statsGuestIP, "sb-a", statsGuestMAC, testNetToken, deny)
	link := r.attach()

	arp := arpRequestFrame()
	refused := synFrame(nil, [4]byte{8, 8, 8, 8}, 443)
	var want tally
	var answers uint64 // frames netd should have sent the guest so far: one reply or reset per send
	// send puts a frame on the link and waits until netd has counted it and its
	// answer has reached the guest. A registration must not change while an
	// answer is in flight, or the answer would be counted against the new one.
	send := func(what string, frame []byte, refusal bool) {
		t.Helper()
		link.send(t, frame)
		want.txPackets++
		want.txBytes += uint64(len(frame))
		if refusal {
			want.policy++
		}
		answers++
		r.settle(what+" ("+want.String()+")", want.matches)
		waitFor(t, "the answer to "+what, func() bool { return link.frames.Load() >= answers })
	}
	send("an ARP request", arp, false)
	send("a refused connect", refused, true)

	// The same guest, pushed again with another policy.
	r.set(statsGuestIP, "sb-a", statsGuestMAC, testNetToken, gateway.NetPolicyWire{Default: "deny", AllowHosts: []string{"x.example"}})
	if g := r.guests(); len(g) != 1 || !want.matches(g[0]) {
		t.Fatalf("after a re-push the report is %+v, want the counters kept (%s)", g, want)
	}
	send("a refused connect after the re-push", refused, true)

	// The address goes to another sandbox: it starts from zero.
	r.set(statsGuestIP, "sb-b", statsGuestMAC, testNetToken, deny)
	if g := r.guests(); len(g) != 1 || g[0].SandboxID != "sb-b" || !(tally{}).matches(g[0]) || g[0].RxPackets != 0 {
		t.Fatalf("a new sandbox on the address inherited counters: %+v", g)
	}
	want = tally{}
	send("an ARP request for the new sandbox", arp, false)

	// Deleting drops the guest from the report. Its link stays up, and frames
	// on it have no registration to be counted against (netd logs the first one).
	r.del(statsGuestIP)
	if g := r.guests(); len(g) != 0 {
		t.Fatalf("a deleted guest is still reported: %+v", g)
	}
	link.send(t, arp)
	waitFor(t, "netd to drop the frame on the revoked link", func() bool {
		return strings.Contains(logs.String(), "netd.spoofed_frame")
	})
	if g := r.guests(); len(g) != 0 {
		t.Fatalf("a frame on a revoked link made the guest reappear: %+v", g)
	}

	// Registered again, even as the same sandbox, it counts from zero, through
	// the same link.
	r.set(statsGuestIP, "sb-b", statsGuestMAC, testNetToken, deny)
	if g := r.guests(); len(g) != 1 || !(tally{}).matches(g[0]) || g[0].RxPackets != 0 {
		t.Fatalf("a re-registered guest kept counters across the delete: %+v", g)
	}
	want = tally{}
	send("an ARP request on the re-registered guest", arp, false)
	r.expect("the report of the re-registered guest ("+want.String()+")", want.matches)
}

func TestStatsAreSortedByAddressAndSurviveJSON(t *testing.T) {
	r := newStatsRig(t, hostProtection{})
	if g := r.guests(); g == nil || len(g) != 0 {
		t.Fatalf("a netd with no guests reports %#v, want an empty list", g)
	}
	// 9 sorts before 10 and 100 only as a number, not as text.
	for i, last := range []int{100, 9, 254, 2, 10} {
		r.set(fmt.Sprintf("100.64.0.%d", last), fmt.Sprintf("sb-%d", last), fmt.Sprintf("52:54:00:00:00:%02x", 0x10+i),
			fmt.Sprintf("%064x", i+1), gateway.NetPolicyWire{Default: "deny"})
	}
	report := queryNetStats(t, r.sock)
	var order []string
	for _, g := range report.Guests {
		order = append(order, g.GuestIP+"/"+g.SandboxID)
		if g.RxBytes+g.RxPackets+g.TxBytes+g.TxPackets != 0 || g.Dropped != (gateway.DropStats{}) {
			t.Errorf("guest %s has traffic though nothing is attached: %+v", g.GuestIP, g)
		}
	}
	wantOrder := []string{"100.64.0.2/sb-2", "100.64.0.9/sb-9", "100.64.0.10/sb-10", "100.64.0.100/sb-100", "100.64.0.254/sb-254"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("report order = %v, want %v", order, wantOrder)
	}
	enc, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var back gateway.NetStats
	if err := json.Unmarshal(enc, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, report) {
		t.Fatalf("JSON round trip changed the report:\n got %+v\nwant %+v", back, report)
	}
}

// The frame path runs for every frame a guest sends or is sent, so counting it
// must cost no allocation, and a nil counter set (a hand-built guestState, the
// unregistered fallback) must be as cheap and as harmless.
func TestCountingDoesNotAllocate(t *testing.T) {
	quiet := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(quiet)

	var c guestCounters
	var none *guestCounters
	for name, f := range map[string]func(){
		"rx":      func() { c.rx(60); none.rx(60) },
		"tx":      func() { c.tx(60); none.tx(60) },
		"spoofed": func() { c.spoofed(); none.spoofed() },
		"denied":  func() { c.denied(true); c.denied(false); none.denied(true) },
	} {
		if n := testing.AllocsPerRun(200, f); n != 0 {
			t.Errorf("%s allocates %v times per call", name, n)
		}
	}

	gw := newTestGateway(t, hostProtection{})
	if err := gw.SetSandbox(statsGuestIP, "sb-a", nil, statsGuestMAC, testNetToken); err != nil {
		t.Fatal(err)
	}
	p := &guestPort{ip: tcpip.AddrFrom4(testGuestIP), mac: testGuestMAC, token: testNetToken}
	spoof := synFrame(&[4]byte{100, 64, 0, 3}, [4]byte{8, 8, 8, 8}, 443)
	for name, frame := range map[string][]byte{
		"IPv4 from the guest": synFrame(nil, [4]byte{8, 8, 8, 8}, 443),
		"ARP from the guest":  arpRequestFrame(),
		"a spoofed frame":     spoof,
	} {
		if n := testing.AllocsPerRun(200, func() { gw.allowIngress(p, frame) }); n != 0 {
			t.Errorf("checking and counting %s allocates %v times per frame", name, n)
		}
	}
}

// Only the egress guard's refusal is a drop. A dial that fails because the
// destination is unreachable or slow is the network's doing, and counting it
// would make a flaky upstream look like a sandbox probing forbidden ranges.
func TestDialFailureCountsOnlyTheGuardsRefusals(t *testing.T) {
	var c guestCounters
	c.dialFailed(&gateway.DeniedError{HardDeny: true})
	c.dialFailed(&gateway.DeniedError{})
	c.dialFailed(fmt.Errorf("dial: %w", &gateway.DeniedError{})) // wrapped
	c.dialFailed(&net.OpError{Op: "dial", Err: errors.New("network is unreachable")})
	c.dialFailed(context.DeadlineExceeded)
	c.dialFailed(nil)
	var none *guestCounters
	none.dialFailed(&gateway.DeniedError{})
	var got gateway.GuestStats
	c.read(&got)
	if want := (gateway.DropStats{Policy: 2, HardDeny: 1}); got.Dropped != want {
		t.Fatalf("dropped = %+v, want %+v", got.Dropped, want)
	}
}

// A datagram netd carries is Tx going out and Rx coming back, never a drop, and
// an upstream that cannot be reached is not a drop either.
func TestForwardedTrafficIsCountedAndAnUnreachableUpstreamIsNotADrop(t *testing.T) {
	echo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		buf := make([]byte, 64)
		for {
			n, from, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			echo.WriteToUDP(append([]byte("re:"), buf[:n]...), from)
		}
	}()

	r := newStatsRig(t, hostProtection{})
	var dials atomic.Int32
	r.gw.hostUDPDial = func(network, address string) (net.Conn, error) {
		if dials.Add(1) == 1 {
			return nil, errors.New("connect: network is unreachable")
		}
		return net.Dial("udp", echo.LocalAddr().String())
	}
	r.set(statsGuestIP, "sb-a", statsGuestMAC, testNetToken, gateway.NetPolicyWire{Default: "public"})
	link := r.attach()
	if err := r.gw.stack.AddStaticNeighbor(nicID, ipv4.ProtocolNumber, tcpip.AddrFrom4(testGuestIP), testGuestMAC); err != nil {
		t.Fatal(err)
	}

	public := [4]byte{8, 8, 8, 8}
	first := udpGuestFrame(testGuestIP, public, testGuestMAC, 45001, 8001, []byte("lost"))
	second := udpGuestFrame(testGuestIP, public, testGuestMAC, 45002, 8001, []byte("ping"))
	link.send(t, first)
	waitFor(t, "the first datagram to be dialed", func() bool { return dials.Load() == 1 })
	link.send(t, second)
	want := tally{txPackets: 2, txBytes: uint64(len(first) + len(second))}
	r.settle("both datagrams counted as Tx ("+want.String()+")", want.matches)
	got := r.expect("the echo to reach the guest, counted as Rx, with the Tx counts unchanged",
		func(g gateway.GuestStats) bool { return want.matches(g) && link.received(g) })
	if got.RxBytes < uint64(header.EthernetMinimumSize+header.IPv4MinimumSize+header.UDPMinimumSize+len("re:ping")) {
		t.Errorf("Rx = %d bytes, less than the echoed datagram", got.RxBytes)
	}
	if !want.matches(got) {
		t.Errorf("counters moved after the traffic settled: %+v, want %s", got, want)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
