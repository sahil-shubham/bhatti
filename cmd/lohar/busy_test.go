//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// A /proc/stat head from a 10-core host: guest time (7172701) is inside user.
const statHostFixture = `cpu  8532035 252 1883194 4820997902 209733 0 20525 0 7172701 0
cpu0 1070221 28 233433 602580324 26254 0 12480 0 896320 0
intr 3012957201 0 9 0 0 0 0 0 0 0
ctxt 8012773094
btime 1792000000
`

// /proc/net/dev from a krucible guest 60 s after boot: eth0 has sent 796 bytes
// and received none, with no traffic from anything (ARP/ND/MLD only).
const netDevGuestFixture = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0
dummy0:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0
  eth0:       0       0    0    0    0     0          0         0      796      10    0    0    0     0       0          0
`

const tcpHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

// tcpRow is a /proc/net/tcp{,6} row for a connection with the given state and
// remote address (hex, as the kernel prints it), in the kernel's column layout.
func tcpRow(state, remote string) string {
	local := "0F02000A:9C40"
	if len(remote) > 13 { // tcp6
		local = "00000000000000000000000000000000:9C40"
	}
	return fmt.Sprintf("   3: %s %s %s 00000000:00000000 02:000AFB23 00000000  1000        0 12014874 2 00000000160ec7ea 21 4 31 10 -1\n", local, remote, state)
}

func TestParseCPUBusy(t *testing.T) {
	for _, c := range []struct {
		name string
		stat string
		busy uint64
		ok   bool
	}{
		// user+nice+system+irq+softirq+steal; idle, iowait and guest left out.
		{"host", statHostFixture, 8532035 + 252 + 1883194 + 0 + 20525 + 0, true},
		{"idle guest", "cpu  0 0 3 2005 8 0 0 0 0 0\ncpu0 0 0 2 1002 4 0 0 0 0 0\n", 3, true},
		{"steal counts", "cpu  10 0 10 100 5 1 2 7 0 0\n", 10 + 10 + 1 + 2 + 7, true},
		{"old kernel, idle last", "cpu  10 2 3 400\n", 15, true},
		{"too few fields", "cpu  10 2 3\n", 0, false},
		{"no trailing newline", "cpu  1 1 1 1 1 1 1 1 1 1", 6, true},
		{"not the cpu line", "cpu0 1 2 3 4 5\n", 0, false},
		{"garbage counter", "cpu  1 x 3 4\n", 0, false},
		{"empty", "", 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			busy, ok := parseCPUBusy([]byte(c.stat))
			if busy != c.busy || ok != c.ok {
				t.Fatalf("parseCPUBusy = %d, %v; want %d, %v", busy, ok, c.busy, c.ok)
			}
		})
	}
}

func TestParseNetDev(t *testing.T) {
	for _, c := range []struct {
		name  string
		dev   string
		iface string
		bytes uint64
		ok    bool
	}{
		{"idle guest", netDevGuestFixture, "eth0", 796, true},
		{"rx and tx add up", strings.Replace(netDevGuestFixture, "  eth0:       0 ", "  eth0:    1000 ", 1), "eth0", 1796, true},
		// Past 99,999,999 bytes the counter fills its column and touches the colon.
		{"counter touches the name", "  eth0:123456789012    8000    0    0    0     0          0         0 98765432101    7000    0    0    0     0       0          0\n", "eth0", 123456789012 + 98765432101, true},
		{"no eth0 (sandbox without network)", strings.Replace(netDevGuestFixture, "  eth0:", " other:", 1), "eth0", 0, false},
		{"another interface", netDevGuestFixture, "lo", 0, true},
		{"truncated line", "  eth0:       0       0    0\n", "eth0", 0, false},
		{"headers only", strings.SplitAfter(netDevGuestFixture, "\n")[0] + strings.SplitAfter(netDevGuestFixture, "\n")[1], "eth0", 0, false},
		{"empty", "", "eth0", 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, ok := parseNetDev([]byte(c.dev), c.iface)
			if n != c.bytes || ok != c.ok {
				t.Fatalf("parseNetDev = %d, %v; want %d, %v", n, ok, c.bytes, c.ok)
			}
		})
	}
}

func TestEstablishedExternal(t *testing.T) {
	for _, c := range []struct {
		name string
		row  string
		want bool
	}{
		// IPv4: 0100007F is 127.0.0.1 — the kernel prints the address as a
		// native-endian word, so a naive left-to-right decode reads 1.0.0.127.
		{"v4 established to 1.1.1.1", tcpRow("01", "01010101:01BB"), true},
		{"v4 established to 10.0.2.2", tcpRow("01", "0202000A:1F90"), true},
		{"v4 established to 128.0.0.1", tcpRow("01", "01000080:0050"), true},
		{"v4 established to 127.0.0.1", tcpRow("01", "0100007F:0400"), false},
		{"v4 established to 127.1.2.3", tcpRow("01", "0302017F:0400"), false},
		{"v4 established to 127.255.255.254", tcpRow("01", "FEFFFF7F:0400"), false},
		// Only ESTABLISHED counts.
		{"v4 listen", tcpRow("0A", "00000000:0000"), false},
		{"v4 syn sent", tcpRow("02", "01010101:01BB"), false},
		{"v4 time wait", tcpRow("06", "01010101:01BB"), false},
		{"v4 close wait", tcpRow("08", "01010101:01BB"), false},
		// IPv6.
		{"v6 listen", tcpRow("0A", "00000000000000000000000000000000:0000"), false},
		{"v6 established to ::1", tcpRow("01", "00000000000000000000000001000000:C350"), false},
		{"v6 established to ::2", tcpRow("01", "00000000000000000000000002000000:C350"), true},
		{"v6 established to 2001:db8::1", tcpRow("01", "B80D0120000000000000000001000000:01BB"), true},
		{"v6 established to fe80::1", tcpRow("01", "000080FE000000000000000001000000:01BB"), true},
		{"v4-mapped 1.1.1.1", tcpRow("01", "0000000000000000FFFF000001010101:01BB"), true},
		{"v4-mapped 127.0.0.1", tcpRow("01", "0000000000000000FFFF00000100007F:0400"), false},
		{"v4-mapped 127.255.255.254", tcpRow("01", "0000000000000000FFFF0000FEFFFF7F:0400"), false},
		// Not rows.
		{"header", tcpHeader, false},
		{"empty", "", false},
		{"garbage address", tcpRow("01", "ZZZZZZZZ:0050"), false},
		{"short address", tcpRow("01", "0101:0050"), false},
		{"no port", tcpRow("01", "01010101"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := establishedExternal([]byte(strings.TrimSuffix(c.row, "\n"))); got != c.want {
				t.Fatalf("establishedExternal(%q) = %v, want %v", c.row, got, c.want)
			}
		})
	}
}

// Tables in the shape of a service host's /proc/net/tcp and tcp6 (addresses
// moved to documentation ranges): listeners, loopback traffic, connections
// that are closing, and — in the second — one live connection to a stranger.
const (
	recordedTCP = tcpHeader +
		"   0: 0100007F:0035 00000000:0000 0A 00000000:00000000 00:00000000 00000000   101        0 563907 1 00000000fd2db2ec 100 0 0 10 10\n" +
		"   1: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 7355 2 00000000d2c77a22 100 0 0 10 0\n" +
		"   2: 0A0200C0:0035 00000000:0000 0A 00000000:00000000 00:00000000 00000000   101        0 563873 1 00000000b98951e1 100 0 0 10 10\n" +
		"   3: 0100007F:C350 0100007F:0400 01 00000000:00000000 00:00000000 00000000  1000        0 20001 1 0000000000000000 100 0 0 10 -1\n" +
		"   4: 0100007F:0400 0100007F:C350 01 00000000:00000000 00:00000000 00000000     0        0 20002 1 0000000000000000 100 0 0 10 -1\n" +
		"   5: 0A0200C0:9C40 077100CB:01BB 06 00000000:00000000 03:0000079D 00000000     0        0 0 3 0000000000000000\n" +
		"   6: 0A0200C0:9C41 077100CB:01BB 08 00000000:00000000 00:00000000 00000000  1000        0 20003 1 0000000000000000 21 4 31 10 -1\n"
	recordedTCP6 = tcpHeader +
		"   0: 00000000000000000000000000000000:0016 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 7357 1 0000000095cf0afe 100 0 0 10 0\n" +
		"   1: 00000000000000000000000000000000:0400 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1036 1 00000000027c4c80 100 0 0 10 0\n" +
		"   2: 0000000000000000FFFF00000100007F:0050 0000000000000000FFFF00000100007F:8ADC 01 00000000:00000000 02:00000202 00000000     0        0 11952115 2 00000000243d3ef6 29 4 29 10 -1\n" +
		"   3: 00000000000000000000000001000000:0050 00000000000000000000000001000000:8AD0 01 00000000:00000000 02:000000CE 00000000     0        0 11981891 2 000000001ea491be 29 4 29 10 -1\n"
	recordedTCPExternal  = "   7: 0A0200C0:9C42 176433C6:01BB 01 00000000:00000000 02:000AFB23 00000000  1000        0 20004 2 00000000160ec7ea 21 4 31 10 -1\n"
	recordedTCP6External = "   4: 0000000000000000FFFF00000A0200C0:0050 0000000000000000FFFF0000077100CB:8ADC 01 00000000:00000000 02:00000202 00000000     0        0 11952116 2 00000000243d3ef7 29 4 29 10 -1\n"
)

func TestExternalConnsOnRecordedTables(t *testing.T) {
	p := newFakeProc(t)
	s := newBusySampler(p.root)
	for _, c := range []struct {
		name       string
		tcp, tcp6  string
		wantExtern bool
	}{
		{"listeners, loopback and closing connections only", recordedTCP, recordedTCP6, false},
		{"plus one live IPv4 connection to a stranger", recordedTCP + recordedTCPExternal, recordedTCP6, true},
		{"plus one live IPv6 (v4-mapped) connection to a stranger", recordedTCP, recordedTCP6 + recordedTCP6External, true},
	} {
		p.write("net/tcp", c.tcp)
		p.write("net/tcp6", c.tcp6)
		if got := s.externalConns(); got != c.wantExtern {
			t.Errorf("%s: externalConns = %v, want %v", c.name, got, c.wantExtern)
		}
	}
}

// fakeProc is a /proc tree on disk. Namespace links are plain symlinks whose
// text is what the kernel's magic links read as.
type fakeProc struct {
	t    *testing.T
	root string
}

const rootNetns = 4026531840

func newFakeProc(t *testing.T) *fakeProc {
	p := &fakeProc{t: t, root: t.TempDir()}
	p.netns("self", rootNetns, tcpHeader, tcpHeader)
	p.write("net/tcp", tcpHeader)
	p.write("net/tcp6", tcpHeader)
	p.setStat(0, 0)
	p.setEth0(0)
	return p
}

func (p *fakeProc) write(rel, content string) {
	p.t.Helper()
	path := filepath.Join(p.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

// netns puts process pid (or "self") in the namespace with that inode, with
// the given /proc/<pid>/net/tcp{,6} tables.
func (p *fakeProc) netns(pid any, inode uint64, tcp, tcp6 string) {
	p.t.Helper()
	dir := filepath.Join(p.root, fmt.Sprint(pid))
	if err := os.MkdirAll(filepath.Join(dir, "ns"), 0o755); err != nil {
		p.t.Fatal(err)
	}
	link := filepath.Join(dir, "ns", "net")
	os.Remove(link)
	if err := os.Symlink(fmt.Sprintf("net:[%d]", inode), link); err != nil {
		p.t.Fatal(err)
	}
	p.write(fmt.Sprintf("%v/net/tcp", pid), tcp)
	p.write(fmt.Sprintf("%v/net/tcp6", pid), tcp6)
}

func (p *fakeProc) setStat(busy, idle uint64) {
	p.t.Helper()
	p.write("stat", fmt.Sprintf("cpu  %d 0 0 %d 0 0 0 0 0 0\nintr 1\n", busy, idle))
}

func (p *fakeProc) setEth0(bytes uint64) {
	p.t.Helper()
	p.write("net/dev", strings.Replace(netDevGuestFixture, "796", fmt.Sprint(bytes), 1))
}

func (p *fakeProc) setRootTCP(rows ...string) {
	p.t.Helper()
	p.write("net/tcp", tcpHeader+strings.Join(rows, ""))
}

func TestExternalEstablishedScansTheWholeTable(t *testing.T) {
	p := newFakeProc(t)
	s := newBusySampler(p.root)
	listen := tcpRow("0A", "00000000:0000")
	external := tcpRow("01", "01010101:01BB")
	// Several buffers of rows; the row of interest is the last one, then
	// the same row split across a buffer boundary.
	rows := strings.Repeat(listen, 2*len(s.buf)/len(listen)+1)
	for name, table := range map[string]string{
		"last row, newline-terminated": tcpHeader + rows + external,
		"last row, unterminated":       tcpHeader + rows + strings.TrimSuffix(external, "\n"),
		"row straddling a buffer": tcpHeader + strings.Repeat(listen, (len(s.buf)-len(tcpHeader)-len(external)/2)/len(listen)) +
			strings.Repeat("x", (len(s.buf)-len(tcpHeader)-len(external)/2)%len(listen)) + "\n" + external,
	} {
		p.write("net/tcp", table)
		if !s.externalEstablished(s.tcp) {
			t.Errorf("%s: missed the external connection in a %d-byte table", name, len(table))
		}
	}
	p.write("net/tcp", tcpHeader+rows)
	if s.externalEstablished(s.tcp) {
		t.Error("a table of listeners reported a connection")
	}
	// A row longer than the buffer is dropped, not a reason to stall or panic.
	p.write("net/tcp", strings.Repeat("y", 3*len(s.buf))+"\n"+external)
	if !s.externalEstablished(s.tcp) {
		t.Error("an oversized junk row hid the rows after it")
	}
}

func TestExternalConnsInAnyNetworkNamespace(t *testing.T) {
	p := newFakeProc(t)
	s := newBusySampler(p.root)
	loopback := tcpRow("01", "0100007F:0400")
	external := tcpRow("01", "01010101:01BB")

	// pid 1 and a service in the root namespace, a container in its own.
	p.netns(1, rootNetns, tcpHeader, tcpHeader)
	p.netns(40, rootNetns, tcpHeader+loopback, tcpHeader)
	p.netns(50, 4026532001, tcpHeader+loopback, tcpHeader)
	p.write("sys/kernel/x", "not a pid")
	p.write("0123/net/tcp", tcpHeader+external) // a leading zero is not a pid
	if s.externalConns() {
		t.Fatal("only loopback connections, yet reported external")
	}

	// The container dials out over IPv6 (v4-mapped): visible only through its namespace.
	p.netns(50, 4026532001, tcpHeader+loopback, tcpHeader+tcpRow("01", "0000000000000000FFFF000001010101:01BB"))
	if !s.externalConns() {
		t.Fatal("missed a connection held in a container's network namespace")
	}

	// Its connection closes while a second process still sits in that namespace.
	p.netns(51, 4026532001, tcpHeader, tcpHeader)
	p.netns(50, 4026532001, tcpHeader+loopback, tcpHeader)
	if s.externalConns() {
		t.Fatal("connection reported after its socket closed")
	}

	// The root namespace counts too.
	p.setRootTCP(external)
	os.RemoveAll(filepath.Join(p.root, "50"))
	if !s.externalConns() {
		t.Fatal("missed a connection in the root namespace")
	}
}

func TestNewNetnsDedupes(t *testing.T) {
	p := newFakeProc(t)
	s := newBusySampler(p.root)
	p.netns(1, rootNetns, tcpHeader, tcpHeader)
	p.netns(50, 4026532001, tcpHeader, tcpHeader)
	p.netns(51, 4026532001, tcpHeader, tcpHeader)
	p.netns(60, 4026532002, tcpHeader, tcpHeader)
	p.write("70/net/tcp", tcpHeader) // exited between the listing and the read: no ns link
	s.seen[rootNetns] = struct{}{}   // externalConns has already read the root namespace

	var got []bool
	for _, pid := range []int{50, 51, 50, 60, 1, 70} {
		got = append(got, s.newNetns(pid))
	}
	if want := []bool{true, false, false, true, false, false}; !slices.Equal(got, want) {
		t.Fatalf("newNetns over pids 50,51,50,60,1,70 = %v; want %v (a namespace is read once, through its first process)", got, want)
	}
}

func TestListPIDs(t *testing.T) {
	p := newFakeProc(t)
	s := newBusySampler(p.root)
	// Enough entries for several getdents calls, in a buffer that holds a few dozen.
	s.dents = make([]byte, 512)
	var want []int
	for pid := 1; pid <= 600; pid += 3 {
		if err := os.MkdirAll(filepath.Join(p.root, fmt.Sprint(pid)), 0o755); err != nil {
			t.Fatal(err)
		}
		want = append(want, pid)
	}
	for _, junk := range []string{"sys", "net", "0", "007", "12x", "-5"} {
		os.MkdirAll(filepath.Join(p.root, junk), 0o755)
	}
	got := slices.Clone(s.listPIDs())
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("listPIDs = %v; want %v", got, want)
	}
	// And again: the scratch slice is reused, not appended to.
	if again := s.listPIDs(); len(again) != len(want) {
		t.Fatalf("second listing has %d pids, want %d", len(again), len(want))
	}
}

func at(sec float64) time.Time {
	return time.Unix(1_700_000_000, 0).Add(time.Duration(sec * float64(time.Second)))
}

func sample(at time.Time, cpu, net uint64) busySample {
	return busySample{at: at, hasCPU: true, cpuBusy: cpu, hasNet: true, netBytes: net}
}

func TestJudgeBusy(t *testing.T) {
	yes := func() bool { return true }
	no := func() bool { return false }
	base := at(0)
	for _, c := range []struct {
		name       string
		prev, cur  busySample
		lastDirect time.Time
		conns      func() bool
		reason     string
		direct     bool
	}{
		{"idle", sample(base, 100, 5000), sample(at(5), 100, 5000), time.Time{}, no, "", false},
		{"idle guest housekeeping", sample(base, 100, 5000), sample(at(5), 102, 5300), time.Time{}, no, "", false},

		// 5% of a vCPU over 5 s is 0.25 s = 25 jiffies at USER_HZ 100.
		{"cpu at the threshold", sample(base, 0, 0), sample(at(5), 25, 0), time.Time{}, no, "", false},
		{"cpu just above", sample(base, 0, 0), sample(at(5), 26, 0), time.Time{}, no, "cpu", true},
		{"cpu saturating", sample(base, 0, 0), sample(at(5), 500, 0), time.Time{}, no, "cpu", true},
		{"cpu threshold follows a longer sample", sample(base, 0, 0), sample(at(10), 50, 0), time.Time{}, no, "", false},
		{"cpu above it over a longer sample", sample(base, 0, 0), sample(at(10), 51, 0), time.Time{}, no, "cpu", true},
		{"cpu counter went backwards", sample(base, 500, 0), sample(at(5), 10, 0), time.Time{}, no, "", false},

		// 2 KiB per 5 s sample.
		{"net at the floor", sample(base, 0, 0), sample(at(5), 0, 2048), time.Time{}, no, "", false},
		{"net just above", sample(base, 0, 0), sample(at(5), 0, 2049), time.Time{}, no, "net", true},
		{"net transfer", sample(base, 0, 0), sample(at(5), 0, 50_000_000), time.Time{}, no, "net", true},
		{"net floor follows a longer sample", sample(base, 0, 0), sample(at(10), 0, 4096), time.Time{}, no, "", false},
		{"net counter went backwards", sample(base, 0, 9000), sample(at(5), 0, 100), time.Time{}, no, "", false},
		{"cpu and net", sample(base, 0, 0), sample(at(5), 60, 9000), time.Time{}, no, "cpu+net", true},

		// A guest without eth0, or one whose /proc/stat could not be read, is judged on what is left.
		{"no eth0, busy cpu", sample(base, 0, 0), busySample{at: at(5), hasCPU: true, cpuBusy: 60}, time.Time{}, no, "cpu", true},
		{"no eth0, idle", sample(base, 0, 0), busySample{at: at(5), hasCPU: true}, time.Time{}, no, "", false},
		{"eth0 appeared since the last sample", busySample{at: base, hasCPU: true}, sample(at(5), 0, 9_000_000), time.Time{}, no, "", false},
		{"eth0 busy, no cpu reading", busySample{at: base, hasNet: true}, busySample{at: at(5), hasNet: true, netBytes: 9000}, time.Time{}, no, "net", true},
		{"no usable counters at all", busySample{at: base}, busySample{at: at(5)}, time.Time{}, no, "", false},

		// An interval that is not positive cannot be judged.
		{"zero interval", sample(base, 0, 0), sample(base, 500, 90000), time.Time{}, no, "", false},
		{"time went backwards", sample(at(5), 0, 0), sample(base, 500, 90000), time.Time{}, no, "", false},

		// Connections alone: only for busyConnGrace after the last CPU/net sample.
		{"connection just after work", sample(base, 0, 0), sample(at(5), 0, 0), at(0), yes, "conn", false},
		{"connection 9 min after work", sample(base, 0, 0), sample(at(600), 0, 0), at(60), yes, "conn", false},
		{"connection exactly at the grace", sample(base, 0, 0), sample(at(600), 0, 0), at(0), yes, "conn", false},
		{"connection past the grace", sample(base, 0, 0), sample(at(601), 0, 0), at(0), yes, "", false},
		{"connection hours after work", sample(base, 0, 0), sample(at(7200), 0, 0), at(0), yes, "", false},
		{"connection but never any work", sample(base, 0, 0), sample(at(5), 0, 0), time.Time{}, yes, "", false},
		{"work within the grace, no connection", sample(base, 0, 0), sample(at(5), 0, 0), at(0), no, "", false},
		{"cpu wins over a connection", sample(base, 0, 0), sample(at(5), 500, 0), at(0), yes, "cpu", true},
		{"net wins over a connection", sample(base, 0, 0), sample(at(5), 0, 90000), at(0), yes, "net", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			reason, direct := judgeBusy(c.prev, c.cur, c.lastDirect, c.conns)
			if reason != c.reason || direct != c.direct {
				t.Fatalf("judgeBusy = %q, %v; want %q, %v", reason, direct, c.reason, c.direct)
			}
		})
	}
}

// The connection probe walks /proc; it must not run when the answer cannot change the verdict.
func TestJudgeBusyOnlyProbesConnectionsWhenTheyDecide(t *testing.T) {
	probes := 0
	conns := func() bool { probes++; return true }
	for name, c := range map[string]struct {
		cur        busySample
		lastDirect time.Time
		probes     int
	}{
		"cpu busy":              {sample(at(5), 500, 0), at(0), 0},
		"net busy":              {sample(at(5), 0, 90000), at(0), 0},
		"grace lapsed":          {sample(at(700), 0, 0), at(0), 0},
		"never any work":        {sample(at(5), 0, 0), time.Time{}, 0},
		"idle within the grace": {sample(at(5), 0, 0), at(0), 1},
	} {
		probes = 0
		judgeBusy(sample(at(0), 0, 0), c.cur, c.lastDirect, conns)
		if probes != c.probes {
			t.Errorf("%s: %d probes, want %d", name, probes, c.probes)
		}
	}
}

// The sampler end to end over a fixture /proc: lastDirect has to survive from
// one tick to the next for the grace to work.
func TestBusySamplerRun(t *testing.T) {
	p := newFakeProc(t)
	s := newBusySampler(p.root)
	start := at(0)
	ticks := make(chan time.Time)
	marks := make(chan string)
	go s.run(start, ticks, func(reason string) { marks <- reason })
	defer close(ticks)
	step := func(sec float64) string {
		t.Helper()
		ticks <- at(sec)
		select {
		case r := <-marks:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("sampler did not answer a tick")
			return ""
		}
	}
	expect := func(sec float64, want, why string) {
		t.Helper()
		if got := step(sec); got != want {
			t.Fatalf("t=%vs (%s): verdict %q, want %q", sec, why, got, want)
		}
	}
	external := tcpRow("01", "01010101:01BB")

	expect(5, "", "nothing happens")
	p.setStat(100, 400) // 1 s of CPU in 5 s
	expect(10, "cpu", "a build starts")
	p.setStat(200, 800)
	expect(15, "cpu", "and keeps going")

	// The build ends; its process leaves a connection to a server behind.
	p.setRootTCP(external)
	expect(20, "conn", "idle but connected, right after work")
	expect(25, "conn", "still")
	expect(15+600, "conn", "ten minutes after the last work")
	expect(15+601, "", "past the grace the connection alone no longer counts")
	expect(15+700, "", "and stays that way")

	// Fresh work renews the grace.
	p.setStat(400, 1200)
	expect(15+705, "cpu", "work again")
	expect(15+710, "conn", "connection counts again")

	// Chatter below the floor is not work; a transfer is.
	p.setRootTCP()
	p.setEth0(90)
	expect(15+715, "", "a few packets of ARP/ND/MLD")
	p.setEth0(90 + 100_000)
	expect(15+720, "net", "a download")
	p.setEth0(90 + 100_000 + 200)
	p.setStat(401, 1600)
	expect(15+725, "", "quiet again, no connections")
}

func TestSamplerSurvivesMissingFiles(t *testing.T) {
	// A sandbox without network has no eth0; early boot or an odd kernel may lack any of these.
	p := newFakeProc(t)
	s := newBusySampler(p.root)
	for _, rel := range []string{"net/dev", "stat", "net/tcp", "net/tcp6", "self"} {
		os.RemoveAll(filepath.Join(p.root, rel))
	}
	a, b := s.sample(at(0)), s.sample(at(5))
	if a.hasCPU || a.hasNet || b.hasCPU || b.hasNet {
		t.Fatalf("samples of an empty /proc claim counters: %+v %+v", a, b)
	}
	if s.externalConns() {
		t.Fatal("empty /proc reported a connection")
	}
	if reason, direct := judgeBusy(a, b, time.Time{}, s.externalConns); reason != "" || direct {
		t.Fatalf("empty /proc judged busy: %q, %v", reason, direct)
	}
	gone := newBusySampler(filepath.Join(p.root, "missing"))
	if gone.externalConns() || gone.listPIDs() != nil {
		t.Fatal("a /proc that does not exist yielded pids or connections")
	}
	// eth0 gone mid-run, then back: no spurious verdict from the gap.
	p.setStat(0, 0)
	p.setEth0(5000)
	c := s.sample(at(10))
	os.Remove(filepath.Join(p.root, "net/dev"))
	d := s.sample(at(15))
	p.setEth0(900_000)
	e := s.sample(at(20))
	if r, _ := judgeBusy(c, d, time.Time{}, func() bool { return false }); r != "" {
		t.Errorf("eth0 vanished: verdict %q", r)
	}
	if r, _ := judgeBusy(d, e, time.Time{}, func() bool { return false }); r != "" {
		t.Errorf("eth0 reappeared with a big counter: verdict %q (no baseline to compare with)", r)
	}
}

func TestActivityFrameCarriesGuestClockAndBusyReason(t *testing.T) {
	oldAct, oldReason := atomic.LoadInt64(&lastActivity), currentBusyReason()
	oldToken := agentToken
	defer func() {
		atomic.StoreInt64(&lastActivity, oldAct)
		busyReason.Store(oldReason)
		agentToken = oldToken
	}()
	agentToken = ""
	atomic.StoreInt64(&lastActivity, 0)

	query := func() (proto.ActivityInfo, string) {
		t.Helper()
		host, guest := net.Pipe()
		defer host.Close()
		go handleControlConnection(guest)
		if err := proto.WriteFrame(host, proto.ACTIVITY_REQ, nil); err != nil {
			t.Fatal(err)
		}
		typ, payload, err := proto.ReadFrame(host)
		if err != nil || typ != proto.ACTIVITY_RESP {
			t.Fatalf("activity reply type 0x%02x, err %v", typ, err)
		}
		var info proto.ActivityInfo
		if err := json.Unmarshal(payload, &info); err != nil {
			t.Fatal(err)
		}
		return info, string(payload)
	}

	markBusy("")
	idle, raw := query()
	if idle.LastActivityUnix != 0 || idle.BusyReason != "" || strings.Contains(raw, "busy_reason") {
		t.Fatalf("a guest that never worked reports %s", raw)
	}
	if d := time.Now().Unix() - idle.NowUnix; d < 0 || d > 2 {
		t.Fatalf("now_unix %d is %ds off the guest clock", idle.NowUnix, d)
	}

	markBusy("cpu")
	busy, raw := query()
	now := time.Now().Unix()
	if busy.BusyReason != "cpu" || now-busy.LastActivityUnix > 2 || busy.LastActivityUnix > now {
		t.Fatalf("a busy sample did not refresh last_activity_unix: %s (now %d)", raw, now)
	}

	// The next sample finds it idle: the reason goes, the stamp stays where it was.
	markBusy("")
	after, _ := query()
	if after.BusyReason != "" || after.LastActivityUnix != busy.LastActivityUnix {
		t.Fatalf("after an idle sample: %+v, was %+v", after, busy)
	}
}

// The parsers against this very kernel, not a recording of one.
func TestSamplerOnRealProc(t *testing.T) {
	s := newBusySampler("/proc")
	a := s.sample(time.Now())
	if !a.hasCPU {
		t.Fatal("could not read the cpu line of /proc/stat")
	}
	for end := time.Now().Add(300 * time.Millisecond); time.Now().Before(end); {
	}
	if b := s.sample(time.Now()); b.cpuBusy <= a.cpuBusy {
		t.Fatalf("busy jiffies did not move across 300 ms of spinning: %d -> %d", a.cpuBusy, b.cpuBusy)
	}

	pids := s.listPIDs()
	if !slices.Contains(pids, 1) || !slices.Contains(pids, os.Getpid()) {
		t.Fatalf("listPIDs missed pid 1 or ourselves among %d pids", len(pids))
	}
	self, ok := s.netnsInode("/proc/self/ns/net")
	viaPID, ok2 := s.netnsInode(fmt.Sprintf("/proc/%d/ns/net", os.Getpid()))
	if !ok || !ok2 || self != viaPID || self == 0 {
		t.Fatalf("namespace inodes %d/%v vs %d/%v", self, ok, viaPID, ok2)
	}
	s.externalConns() // whatever this host holds; it must just not misbehave
}

func BenchmarkBusySample(b *testing.B) {
	s := newBusySampler("/proc")
	b.ReportAllocs()
	for range b.N {
		s.sample(time.Now())
	}
}

// The walk of every process: root tables empty, so a connection held by the
// host running the benchmark cannot end it early.
func BenchmarkExternalConnsWalk(b *testing.B) {
	s := newBusySampler("/proc")
	empty := filepath.Join(b.TempDir(), "tcp")
	if err := os.WriteFile(empty, []byte(tcpHeader), 0o644); err != nil {
		b.Fatal(err)
	}
	s.tcp, s.tcp6 = empty, empty
	b.ReportMetric(float64(len(s.listPIDs())), "pids")
	b.ReportAllocs()
	for range b.N {
		s.externalConns()
	}
}
