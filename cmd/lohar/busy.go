//go:build linux

package main

import (
	"bytes"
	"encoding/binary"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

// The host pauses a sandbox that looks idle for 30 s, judging by host requests
// and lastActivity. A guest working on its own (a CI job whose exec has
// returned, an agent streaming a reply) makes no host requests, so the sampler
// below stamps lastActivity for it when CPU, network or open connections show
// real work.

const (
	// Well under the host's 30 s warm timeout: one late sample cannot let a busy
	// guest look idle.
	busySampleInterval = 5 * time.Second

	// Share of one vCPU, all CPUs summed. An idle guest measured 0 busy jiffies
	// per 10 s; any build, test run or agent loop is far above this.
	busyCPUFraction = 0.05

	// /proc/stat counts in USER_HZ: 100 on Linux x86 and arm64 whatever
	// CONFIG_HZ is (the lean kernels use 250 and 100); 100 jiffies/s/vCPU
	// measured in a guest.
	userHZ = 100

	// eth0 bytes per sample interval. An idle guest still sends a few 90-byte
	// packets a minute (ARP, ND, MLD) and idle connections swap keepalives;
	// counting those would keep every VM hot forever.
	busyNetBytes = 2 << 10

	// Open connections alone keep a guest busy this long after its last CPU or
	// network work: an agent waiting on a slow reply holds one with nothing
	// moving, but an idle connection pool must not pin a VM hot indefinitely.
	busyConnGrace = 10 * time.Minute

	// The host reaches the agent over vsock, so none of lohar's own traffic is
	// on this interface.
	busyIface = "eth0"
)

// Why a sample judged the guest busy.
const (
	busyCPU  = "cpu"
	busyNet  = "net"
	busyBoth = "cpu+net"
	busyConn = "conn"
)

// busySample is one reading of the guest's activity counters.
type busySample struct {
	at       time.Time // monotonic: elapsed time survives wall-clock steps
	hasCPU   bool
	cpuBusy  uint64 // /proc/stat jiffies, all CPUs, minus idle and iowait
	hasNet   bool
	netBytes uint64 // busyIface rx+tx
}

// judgeBusy says whether the guest worked between prev and cur, and why. direct
// is true when CPU or network showed it: only those renew lastDirect, the time
// of the last such sample. Open connections (conns walks /proc, so it is asked
// only when it can matter) keep a guest busy for busyConnGrace after lastDirect.
func judgeBusy(prev, cur busySample, lastDirect time.Time, conns func() bool) (reason string, direct bool) {
	dt := cur.at.Sub(prev.at).Seconds()
	cpu := dt > 0 && prev.hasCPU && cur.hasCPU && cur.cpuBusy >= prev.cpuBusy &&
		float64(cur.cpuBusy-prev.cpuBusy) > busyCPUFraction*userHZ*dt
	net := dt > 0 && prev.hasNet && cur.hasNet && cur.netBytes >= prev.netBytes &&
		float64(cur.netBytes-prev.netBytes) > busyNetBytes*dt/busySampleInterval.Seconds()
	switch {
	case cpu && net:
		return busyBoth, true
	case cpu:
		return busyCPU, true
	case net:
		return busyNet, true
	case !lastDirect.IsZero() && cur.at.Sub(lastDirect) <= busyConnGrace && conns():
		return busyConn, false
	}
	return "", false
}

// busyReason is the latest sample's verdict, for ActivityInfo.
var busyReason atomic.Value // string

// markBusy records a sample's verdict. A busy guest refreshes lastActivity just
// as a host request does.
func markBusy(reason string) {
	busyReason.Store(reason)
	if reason != "" {
		updateActivity()
	}
}

func currentBusyReason() string {
	r, _ := busyReason.Load().(string)
	return r
}

// startActivitySampler samples for the life of the agent.
func startActivitySampler() {
	go func() {
		// lohar is PID 1: a bug here must cost the sampler, not the VM.
		defer func() {
			if r := recover(); r != nil {
				logf("activity sampler stopped: %v", r)
			}
		}()
		t := time.NewTicker(busySampleInterval)
		defer t.Stop()
		newBusySampler("/proc").run(time.Now(), t.C, markBusy)
	}()
}

// busySampler reads the counters judgeBusy needs. Its buffers are reused, so a
// sample costs a few small allocations however many processes run.
type busySampler struct {
	// procfs paths, fixed at construction; tests point them at a fixture tree.
	proc, stat, netDev, tcp, tcp6, selfNetNS string

	buf   []byte // file reads
	dents []byte // getdents
	path  []byte // per-pid paths
	pids  []int
	seen  map[uint64]struct{} // network namespaces already read in this walk
}

func newBusySampler(proc string) *busySampler {
	return &busySampler{
		proc: proc, stat: proc + "/stat", netDev: proc + "/net/dev",
		tcp: proc + "/net/tcp", tcp6: proc + "/net/tcp6", selfNetNS: proc + "/self/ns/net",
		buf: make([]byte, 32<<10), dents: make([]byte, 8<<10), seen: make(map[uint64]struct{}),
	}
}

// run takes a sample at every tick and reports each verdict through mark.
func (s *busySampler) run(start time.Time, ticks <-chan time.Time, mark func(reason string)) {
	prev := s.sample(start)
	var lastDirect time.Time
	for at := range ticks {
		cur := s.sample(at)
		reason, direct := judgeBusy(prev, cur, lastDirect, s.externalConns)
		if direct {
			lastDirect = cur.at
		}
		mark(reason)
		prev = cur
	}
}

// sample reads the cheap counters. A file that is missing or unreadable (no
// eth0 in a sandbox without network) leaves its signal out of the sample.
func (s *busySampler) sample(at time.Time) busySample {
	cur := busySample{at: at}
	if b, ok := s.read(s.stat, 512); ok { // the cpu line is first; no need for the rest
		cur.cpuBusy, cur.hasCPU = parseCPUBusy(b)
	}
	if b, ok := s.read(s.netDev, len(s.buf)); ok {
		cur.netBytes, cur.hasNet = parseNetDev(b, busyIface)
	}
	return cur
}

// read returns up to limit bytes of a procfs file, in s.buf.
func (s *busySampler) read(path string, limit int) ([]byte, bool) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, false
	}
	defer syscall.Close(fd)
	buf := s.buf[:limit]
	n := 0
	for n < len(buf) {
		m, err := syscall.Read(fd, buf[n:])
		if err != nil {
			return nil, false
		}
		if m == 0 {
			break
		}
		n += m
	}
	return buf[:n], true
}

// externalConns reports an ESTABLISHED TCP connection to a non-loopback peer in
// any network namespace. Containers keep theirs in namespaces of their own, so
// after the root one, each distinct namespace is read once, through any of its
// processes.
func (s *busySampler) externalConns() bool {
	if s.externalEstablished(s.tcp) || s.externalEstablished(s.tcp6) {
		return true
	}
	clear(s.seen)
	if ino, ok := s.netnsInode(s.selfNetNS); ok {
		s.seen[ino] = struct{}{}
	}
	for _, pid := range s.listPIDs() {
		if !s.newNetns(pid) {
			continue
		}
		dir := s.pidPath(pid, "/net/")
		if s.externalEstablished(dir+"tcp") || s.externalEstablished(dir+"tcp6") {
			return true
		}
	}
	return false
}

// newNetns reports whether pid's network namespace has not been read in this
// walk, and marks it read. A process that exited since the listing is skipped.
func (s *busySampler) newNetns(pid int) bool {
	ino, ok := s.netnsInode(s.pidPath(pid, "/ns/net"))
	if !ok {
		return false
	}
	if _, dup := s.seen[ino]; dup {
		return false
	}
	s.seen[ino] = struct{}{}
	return true
}

func (s *busySampler) pidPath(pid int, tail string) string {
	b := append(s.path[:0], s.proc...)
	b = append(b, '/')
	b = strconv.AppendInt(b, int64(pid), 10)
	b = append(b, tail...)
	s.path = b
	return string(b)
}

// netnsInode identifies the namespace a /proc/<pid>/ns/net link points at,
// which reads like "net:[4026531840]".
func (s *busySampler) netnsInode(link string) (uint64, bool) {
	var b [64]byte
	n, err := syscall.Readlink(link, b[:])
	if err != nil || n <= 0 {
		return 0, false
	}
	l := b[:n]
	open := bytes.IndexByte(l, '[')
	if open < 0 || l[n-1] != ']' {
		return 0, false
	}
	ino, err := strconv.ParseUint(string(l[open+1:n-1]), 10, 64)
	return ino, err == nil
}

// linux_dirent64: d_ino u64, d_off s64, d_reclen u16, d_type u8, d_name.
const (
	direntReclen = 16
	direntName   = 19
)

// listPIDs lists the numeric entries of /proc into s.pids. Parsing getdents
// by hand spares an allocation per entry (and os.ReadDir's sort).
func (s *busySampler) listPIDs() []int {
	s.pids = s.pids[:0]
	fd, err := syscall.Open(s.proc, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil
	}
	defer syscall.Close(fd)
	for {
		n, err := syscall.ReadDirent(fd, s.dents)
		if err != nil || n <= 0 {
			return s.pids
		}
		for b := s.dents[:n]; len(b) >= direntName; {
			reclen := int(binary.NativeEndian.Uint16(b[direntReclen:]))
			if reclen < direntName || reclen > len(b) {
				break
			}
			name := b[direntName:reclen]
			if i := bytes.IndexByte(name, 0); i >= 0 {
				name = name[:i]
			}
			if pid, ok := parsePID(name); ok {
				s.pids = append(s.pids, pid)
			}
			b = b[reclen:]
		}
	}
}

func parsePID(name []byte) (int, bool) {
	if len(name) == 0 || len(name) > 10 || name[0] == '0' {
		return 0, false
	}
	pid := 0
	for _, c := range name {
		if c < '0' || c > '9' {
			return 0, false
		}
		pid = pid*10 + int(c-'0')
	}
	return pid, true
}

// externalEstablished scans one /proc/net/tcp{,6} table, a chunk at a time,
// for an ESTABLISHED connection to a non-loopback peer.
func (s *busySampler) externalEstablished(path string) bool {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(fd)
	buf := s.buf
	have := 0 // an unfinished row carried over from the last read, at buf[:have]
	for {
		n, err := syscall.Read(fd, buf[have:])
		if err != nil || n == 0 {
			return have > 0 && establishedExternal(buf[:have])
		}
		end := have + n
		start := 0
		for {
			i := bytes.IndexByte(buf[start:end], '\n')
			if i < 0 {
				break
			}
			if establishedExternal(buf[start : start+i]) {
				return true
			}
			start += i + 1
		}
		have = copy(buf, buf[start:end])
		if have == len(buf) { // a whole buffer without a newline is not a table
			have = 0
		}
	}
}

// parseCPUBusy sums the aggregate cpu line of /proc/stat: user, nice, system,
// irq, softirq and steal. Idle and iowait are the CPU waiting; guest and
// guest_nice are already inside user and nice.
func parseCPUBusy(stat []byte) (busy uint64, ok bool) {
	line := stat
	if i := bytes.IndexByte(stat, '\n'); i >= 0 {
		line = stat[:i]
	}
	name, rest := nextField(line)
	if string(name) != "cpu" {
		return 0, false
	}
	for i := range 8 { // user nice system idle iowait irq softirq steal
		var f []byte
		f, rest = nextField(rest)
		if len(f) == 0 {
			if i < 4 {
				return 0, false
			}
			return busy, true // an old kernel stops at idle
		}
		v, err := strconv.ParseUint(string(f), 10, 64)
		if err != nil {
			return 0, false
		}
		if i != 3 && i != 4 {
			busy += v
		}
	}
	return busy, true
}

// parseNetDev returns iface's rx+tx byte counters from /proc/net/dev. The name
// and the first counter can touch ("eth0:123"), so the line splits at the colon.
func parseNetDev(dev []byte, iface string) (uint64, bool) {
	for len(dev) > 0 {
		line := dev
		if i := bytes.IndexByte(dev, '\n'); i >= 0 {
			line, dev = dev[:i], dev[i+1:]
		} else {
			dev = nil
		}
		colon := bytes.IndexByte(line, ':')
		if colon < 0 || string(bytes.TrimSpace(line[:colon])) != iface {
			continue
		}
		rest := line[colon+1:]
		var rx, tx uint64
		for i := range 9 { // rx: bytes packets errs drop fifo frame compressed multicast, tx: bytes
			var f []byte
			f, rest = nextField(rest)
			v, err := strconv.ParseUint(string(f), 10, 64)
			if err != nil {
				return 0, false
			}
			switch i {
			case 0:
				rx = v
			case 8:
				tx = v
			}
		}
		return rx + tx, true
	}
	return 0, false
}

// establishedExternal reports whether a /proc/net/tcp{,6} row is an
// ESTABLISHED (01) connection to a peer that is not loopback.
func establishedExternal(row []byte) bool {
	var remote, state []byte
	_, row = nextField(row) // sl
	_, row = nextField(row) // local_address
	remote, row = nextField(row)
	state, _ = nextField(row)
	if len(state) != 2 || state[0] != '0' || state[1] != '1' {
		return false
	}
	colon := bytes.IndexByte(remote, ':')
	return colon >= 0 && externalPeer(remote[:colon])
}

// externalPeer reports whether a hex address from /proc/net/tcp{,6} is anything
// but loopback: 127.0.0.0/8, ::1, or either of those v4-mapped. The kernel
// prints each 32-bit word of the address as a native-endian number, so the
// words go through NativeEndian to get back to network order.
func externalPeer(hex []byte) bool {
	if len(hex) != 8 && len(hex) != 32 {
		return false
	}
	var raw [16]byte
	for w := range len(hex) / 8 {
		var v uint32
		for _, c := range hex[w*8 : w*8+8] {
			d, ok := hexDigit(c)
			if !ok {
				return false
			}
			v = v<<4 | uint32(d)
		}
		binary.NativeEndian.PutUint32(raw[w*4:], v)
	}
	addr := raw[:len(hex)/2]
	if len(addr) == 16 {
		if !bytes.Equal(addr[:12], v4MappedPrefix[:]) {
			return !bytes.Equal(addr, v6Loopback[:])
		}
		addr = addr[12:]
	}
	return addr[0] != 127
}

var (
	v4MappedPrefix = [12]byte{10: 0xff, 11: 0xff}
	v6Loopback     = [16]byte{15: 1}
)

func hexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// nextField splits off the first whitespace-delimited field of b.
func nextField(b []byte) (field, rest []byte) {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t') {
		i++
	}
	j := i
	for j < len(b) && b[j] != ' ' && b[j] != '\t' {
		j++
	}
	return b[i:j], b[j:]
}
