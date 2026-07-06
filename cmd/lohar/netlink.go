//go:build linux

package main

import (
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

// This file configures eth0 via rtnetlink directly — no `ip` binary and no
// kernel IP autoconfig (CONFIG_IP_PNP is off in our kernels), so it works in the
// minimal box rootfs and in imported OCI images alike. Used by the virtio-net /
// gateway path (DESIGN-bhatti-v2-networking §0c); lohar reads the addressing
// from the config drive.

// configureEth0 brings up the interface, assigns ipCIDR (e.g. "100.64.0.2/24"),
// and adds a default route via gateway (e.g. "100.64.0.1").
func configureEth0(name, ipCIDR, gateway string) error {
	ip, ipnet, err := net.ParseCIDR(ipCIDR)
	if err != nil {
		return fmt.Errorf("parse %q: %w", ipCIDR, err)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("only IPv4 supported for now: %q", ipCIDR)
	}
	prefix, _ := ipnet.Mask.Size()

	iface, err := net.InterfaceByName(name)
	if err != nil {
		return fmt.Errorf("interface %s: %w", name, err)
	}
	idx := iface.Index

	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("netlink socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("netlink bind: %w", err)
	}

	if err := linkUp(fd, idx); err != nil {
		return fmt.Errorf("link up: %w", err)
	}
	if err := addAddr(fd, idx, ip4, prefix); err != nil {
		return fmt.Errorf("add addr: %w", err)
	}
	if gateway != "" {
		gw := net.ParseIP(gateway).To4()
		if gw == nil {
			return fmt.Errorf("bad gateway %q", gateway)
		}
		if err := addDefaultRoute(fd, idx, gw); err != nil {
			return fmt.Errorf("add route: %w", err)
		}
	}
	return nil
}

var nlSeq uint32

// nlRequest sends one netlink message (payload already includes its rtattrs) and
// waits for the ACK, returning any kernel error.
func nlRequest(fd int, msgType uint16, flags uint16, payload []byte) error {
	nlSeq++
	hdr := unix.NlMsghdr{
		Len:   uint32(unix.SizeofNlMsghdr + len(payload)),
		Type:  msgType,
		Flags: unix.NLM_F_REQUEST | unix.NLM_F_ACK | flags,
		Seq:   nlSeq,
	}
	buf := make([]byte, 0, hdr.Len)
	buf = append(buf, (*(*[unix.SizeofNlMsghdr]byte)(unsafe.Pointer(&hdr)))[:]...)
	buf = append(buf, payload...)

	if err := unix.Sendto(fd, buf, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}

	resp := make([]byte, 4096)
	n, _, err := unix.Recvfrom(fd, resp, 0)
	if err != nil {
		return err
	}
	if n < unix.SizeofNlMsghdr {
		return fmt.Errorf("short netlink response (%d bytes)", n)
	}
	rhdr := (*unix.NlMsghdr)(unsafe.Pointer(&resp[0]))
	if rhdr.Type == unix.NLMSG_ERROR {
		// NLMSG_ERROR payload starts with an int32 errno (0 == ACK).
		errno := *(*int32)(unsafe.Pointer(&resp[unix.SizeofNlMsghdr]))
		if errno != 0 {
			return unix.Errno(-errno)
		}
	}
	return nil
}

// align4 rounds up to the netlink 4-byte alignment.
func align4(n int) int { return (n + 3) &^ 3 }

// attr encodes one rtattr TLV (with padding).
func attr(typ uint16, data []byte) []byte {
	l := unix.SizeofRtAttr + len(data)
	b := make([]byte, align4(l))
	*(*uint16)(unsafe.Pointer(&b[0])) = uint16(l)
	*(*uint16)(unsafe.Pointer(&b[2])) = typ
	copy(b[unix.SizeofRtAttr:], data)
	return b
}

func linkUp(fd, idx int) error {
	msg := unix.IfInfomsg{Family: unix.AF_UNSPEC, Index: int32(idx), Flags: unix.IFF_UP, Change: unix.IFF_UP}
	payload := (*(*[unix.SizeofIfInfomsg]byte)(unsafe.Pointer(&msg)))[:]
	return nlRequest(fd, unix.RTM_NEWLINK, 0, payload)
}

func addAddr(fd, idx int, ip4 net.IP, prefix int) error {
	msg := unix.IfAddrmsg{Family: unix.AF_INET, Prefixlen: uint8(prefix), Scope: unix.RT_SCOPE_UNIVERSE, Index: uint32(idx)}
	payload := append([]byte{}, (*(*[unix.SizeofIfAddrmsg]byte)(unsafe.Pointer(&msg)))[:]...)
	payload = append(payload, attr(unix.IFA_LOCAL, ip4)...)
	payload = append(payload, attr(unix.IFA_ADDRESS, ip4)...)
	return nlRequest(fd, unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_EXCL, payload)
}

func addDefaultRoute(fd, idx int, gw net.IP) error {
	msg := unix.RtMsg{
		Family:   unix.AF_INET,
		Dst_len:  0,
		Table:    unix.RT_TABLE_MAIN,
		Protocol: unix.RTPROT_BOOT,
		Scope:    unix.RT_SCOPE_UNIVERSE,
		Type:     unix.RTN_UNICAST,
	}
	payload := append([]byte{}, (*(*[unix.SizeofRtMsg]byte)(unsafe.Pointer(&msg)))[:]...)
	payload = append(payload, attr(unix.RTA_GATEWAY, gw)...)
	oif := make([]byte, 4)
	*(*uint32)(unsafe.Pointer(&oif[0])) = uint32(idx)
	payload = append(payload, attr(unix.RTA_OIF, oif)...)
	return nlRequest(fd, unix.RTM_NEWROUTE, unix.NLM_F_CREATE, payload)
}
