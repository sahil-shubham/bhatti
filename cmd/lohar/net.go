//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// AF_VSOCK constants.
const (
	AF_VSOCK        = 40
	VMADDR_CID_ANY  = 0xFFFFFFFF
	VMADDR_CID_HOST = 2 // the host's well-known vsock CID
)

// dialVsock connects to the host (VMADDR_CID_HOST) on a vsock port — the
// guest→host direction, used at boot to fetch config (§3.4). libkrun bridges it
// to the daemon's UDS (krun_add_vsock_port2 listen=false).
func dialVsock(port uint32) (net.Conn, error) {
	fd, err := syscall.Socket(AF_VSOCK, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	addr := SockaddrVM{Family: AF_VSOCK, Port: port, CID: VMADDR_CID_HOST}
	if _, _, errno := syscall.Syscall(syscall.SYS_CONNECT, uintptr(fd),
		uintptr(unsafe.Pointer(&addr)), unsafe.Sizeof(addr)); errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("vsock connect (cid=%d port=%d): %v", VMADDR_CID_HOST, port, errno)
	}
	return &vsockConn{file: os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d", port))}, nil
}

// SockaddrVM matches struct sockaddr_vm (16 bytes).
type SockaddrVM struct {
	Family    uint16
	Reserved1 uint16
	Port      uint32
	CID       uint32
	Flags     uint8
	Zero      [3]uint8
}

func listenVsock(port uint32) (net.Listener, error) {
	// BLOCKING socket with a receive timeout. The timeout makes accept4
	// return EAGAIN periodically (kernel-level timer, not Go runtime).
	// This is essential for snapshot/resume: after restore, Go's runtime
	// (timers, netpoller, goroutine scheduler) is stalled. The kernel
	// timer is the only thing that fires reliably, giving us a heartbeat
	// to detect resume (via clock jump) and recreate the listener.
	fd, err := syscall.Socket(AF_VSOCK, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}

	addr := SockaddrVM{
		Family: AF_VSOCK,
		Port:   port,
		CID:    VMADDR_CID_ANY,
	}
	_, _, errno := syscall.Syscall(syscall.SYS_BIND, uintptr(fd),
		uintptr(unsafe.Pointer(&addr)), unsafe.Sizeof(addr))
	if errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("vsock bind port %d: %v", port, errno)
	}

	if err := syscall.Listen(fd, 128); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("vsock listen: %w", err)
	}

	// Set accept timeout so accept4 returns EAGAIN every second.
	// This kernel-level timer fires even when Go's runtime is stalled
	// (as happens after Firecracker snapshot/resume).
	tv := syscall.Timeval{Sec: 1}
	syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)

	return &vsockListener{fd: fd, port: port}, nil
}

// vsockListener implements net.Listener for AF_VSOCK using a blocking socket.
// The accept4 syscall blocks in the kernel, which survives snapshot/resume.
type vsockListener struct {
	fd   int
	port uint32
}

func (l *vsockListener) Accept() (net.Conn, error) {
	for {
		before := time.Now()
		nfd, _, errno := syscall.Syscall6(syscall.SYS_ACCEPT4,
			uintptr(l.fd), 0, 0,
			uintptr(syscall.SOCK_CLOEXEC), 0, 0)
		if errno == 0 {
			f := os.NewFile(nfd, fmt.Sprintf("vsock-conn:%d", l.port))
			return &vsockConn{file: f}, nil
		}
		if errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK {
			// accept4 timed out (SO_RCVTIMEO). Check for resume:
			// if wall clock jumped far beyond our 1-second timeout,
			// the VM was paused and resumed.
			elapsed := time.Since(before)
			if elapsed > 3*time.Second {
				logf("vsock port %d: resume detected (accept took %v), re-creating listener", l.port, elapsed)
				time.Sleep(200 * time.Millisecond) // let transport reset settle
				if err := l.recreate(); err != nil {
					logf("vsock port %d: re-create failed: %v", l.port, err)
				} else {
					logf("vsock port %d: listener re-created", l.port)
				}
			}
			continue
		}
		return nil, fmt.Errorf("vsock accept: %v", errno)
	}
}

// recreate closes the current fd and creates a fresh vsock listener.
func (l *vsockListener) recreate() error {
	syscall.Close(l.fd)

	fd, err := syscall.Socket(AF_VSOCK, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)

	addr := SockaddrVM{Family: AF_VSOCK, Port: l.port, CID: VMADDR_CID_ANY}
	_, _, errno := syscall.Syscall(syscall.SYS_BIND, uintptr(fd),
		uintptr(unsafe.Pointer(&addr)), unsafe.Sizeof(addr))
	if errno != 0 {
		syscall.Close(fd)
		return fmt.Errorf("bind: %v", errno)
	}
	if err := syscall.Listen(fd, 128); err != nil {
		syscall.Close(fd)
		return err
	}
	tv := syscall.Timeval{Sec: 1}
	syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)

	l.fd = fd
	return nil
}

func (l *vsockListener) Close() error {
	return syscall.Close(l.fd)
}

func (l *vsockListener) Addr() net.Addr {
	return vsockAddr(l.port)
}

// vsockConn wraps an os.File as a net.Conn for AF_VSOCK connections.
type vsockConn struct {
	file *os.File
}

func (c *vsockConn) Read(b []byte) (int, error)         { return c.file.Read(b) }
func (c *vsockConn) Write(b []byte) (int, error)        { return c.file.Write(b) }
func (c *vsockConn) Close() error                       { return c.file.Close() }
func (c *vsockConn) LocalAddr() net.Addr                { return vsockAddr(0) }
func (c *vsockConn) RemoteAddr() net.Addr               { return vsockAddr(0) }
func (c *vsockConn) SetDeadline(t time.Time) error      { return c.file.SetDeadline(t) }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return c.file.SetReadDeadline(t) }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return c.file.SetWriteDeadline(t) }

type vsockAddr uint32

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return fmt.Sprintf("vsock://:%d", uint32(a)) }

// Networking ioctl constants.
const (
	SIOCGIFFLAGS = 0x8913
	SIOCSIFFLAGS = 0x8914
	IFF_UP       = 0x1
	IFF_RUNNING  = 0x40
)

func bringUpInterface(name string) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		logf("socket for %s: %v", name, err)
		return
	}
	defer syscall.Close(fd)

	var ifr [40]byte
	copy(ifr[:], name)

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		SIOCGIFFLAGS, uintptr(unsafe.Pointer(&ifr[0]))); errno != 0 {
		logf("SIOCGIFFLAGS %s: %v", name, errno)
		return
	}

	flags := binary.LittleEndian.Uint16(ifr[16:18])
	flags |= IFF_UP | IFF_RUNNING
	binary.LittleEndian.PutUint16(ifr[16:18], flags)

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		SIOCSIFFLAGS, uintptr(unsafe.Pointer(&ifr[0]))); errno != 0 {
		logf("SIOCSIFFLAGS %s: %v", name, errno)
	}
}

// setupNetworking configures eth0 from the kernel ip= cmdline parameter.
// This makes lohar work with ANY kernel — no CONFIG_IP_PNP required.
//
// Format: ip=<client-ip>::<gateway>:<netmask>::<device>:off:<dns1>:<dns2>:
// Example: ip=192.168.137.2::192.168.137.1:255.255.255.0::eth0:off:1.1.1.1:8.8.8.8:
func setupNetworking() {
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		logf("cannot read /proc/cmdline: %v", err)
		return
	}

	// Parse ip= parameter
	var ipParam string
	for _, field := range strings.Fields(string(cmdline)) {
		if strings.HasPrefix(field, "ip=") {
			ipParam = strings.TrimPrefix(field, "ip=")
			break
		}
	}
	if ipParam == "" {
		logf("no ip= in cmdline, skipping network setup")
		return
	}

	// Parse fields: client-ip::gateway:netmask::device:...
	parts := strings.Split(ipParam, ":")
	if len(parts) < 4 {
		logf("ip= parameter too short: %q", ipParam)
		return
	}
	clientIP := parts[0]
	gateway := parts[2]
	netmask := parts[3]
	device := "eth0"
	if len(parts) > 5 && parts[5] != "" {
		device = parts[5]
	}

	if clientIP == "" || netmask == "" {
		logf("ip= missing client IP or netmask: %q", ipParam)
		return
	}

	// Check if the kernel already configured the interface (CONFIG_IP_PNP=y)
	iface, err := net.InterfaceByName(device)
	if err == nil {
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			if strings.HasPrefix(addr.String(), clientIP+"/") {
				logf("net: %s: %s (kernel-configured)", device, addr)
				return
			}
		}
	}

	// Kernel didn't configure it — do it ourselves via ip commands
	logf("net: configuring %s %s/%s gw %s", device, clientIP, netmask, gateway)

	// Convert dotted netmask to CIDR prefix length
	prefix := netmaskToCIDR(netmask)

	bringUpInterface(device)
	if err := run("ip", "addr", "add", fmt.Sprintf("%s/%d", clientIP, prefix), "dev", device); err != nil {
		logf("ip addr add: %v", err)
		return
	}
	if gateway != "" {
		if err := run("ip", "route", "add", "default", "via", gateway, "dev", device); err != nil {
			logf("ip route add: %v", err)
		}
	}

	logf("net: %s: %s/%d", device, clientIP, prefix)
}

// netmaskToCIDR converts "255.255.255.0" to 24.
func netmaskToCIDR(mask string) int {
	ip := net.ParseIP(mask)
	if ip == nil {
		return 24
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return 24
	}
	ones, _ := net.IPv4Mask(ip4[0], ip4[1], ip4[2], ip4[3]).Size()
	return ones
}

// run executes a command and returns an error if it fails.
func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %s: %w", name, args, strings.TrimSpace(string(out)), err)
	}
	return nil
}
