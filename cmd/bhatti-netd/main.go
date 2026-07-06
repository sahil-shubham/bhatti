package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/sahil-shubham/bhatti/pkg/gateway"

	"gvisor.dev/gvisor/pkg/tcpip"
)

// bhatti-netd --net-uds <path> --gw-ip 100.64.0.1 --prefix 24 --mac 52:54:00:00:00:01
//
// netd LISTENS on the unixstream socket; libkrun's virtio-net backend CONNECTS
// to it (net/unixstream.rs Unixstream::open → connect). The daemon spawns one
// netd per owner, then starts the VM pointed at the same path via
// krun_add_net_unixstream. Egress policy / substitution / inbound layer on later.
func main() {
	netUDS := flag.String("net-uds", "", "unixstream socket to LISTEN on (libkrun connects here)")
	gwIP := flag.String("gw-ip", "100.64.0.1", "gateway IPv4 address")
	prefix := flag.Int("prefix", 24, "gateway subnet prefix length")
	macStr := flag.String("mac", "52:54:00:00:00:01", "gateway link (MAC) address")
	flag.Parse()

	if *netUDS == "" {
		log.Fatal("bhatti-netd: --net-uds is required")
	}
	cfg, err := parseConfig(*gwIP, *prefix, *macStr)
	if err != nil {
		log.Fatalf("bhatti-netd: %v", err)
	}

	_ = os.Remove(*netUDS) // clear any stale socket from a prior incarnation
	ln, err := net.Listen("unix", *netUDS)
	if err != nil {
		log.Fatalf("bhatti-netd: listen %s: %v", *netUDS, err)
	}
	defer ln.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	log.Printf("bhatti-netd: listening on %s (gw %s/%d, mac %s)", *netUDS, *gwIP, *prefix, *macStr)
	if err := serve(ctx, ln, cfg); err != nil && ctx.Err() == nil {
		log.Fatalf("bhatti-netd: %v", err)
	}
}

// gwConfig is the parsed gateway addressing.
type gwConfig struct {
	ip     tcpip.Address
	prefix int
	mac    tcpip.LinkAddress
}

func parseConfig(gwIP string, prefix int, macStr string) (gwConfig, error) {
	mac, err := net.ParseMAC(macStr)
	if err != nil {
		return gwConfig{}, err
	}
	ip := net.ParseIP(gwIP).To4()
	if ip == nil {
		return gwConfig{}, &net.ParseError{Type: "IPv4 address", Text: gwIP}
	}
	return gwConfig{
		ip:     tcpip.AddrFrom4([4]byte{ip[0], ip[1], ip[2], ip[3]}),
		prefix: prefix,
		mac:    tcpip.LinkAddress(mac),
	}, nil
}

// serve accepts the VM's connection and runs the gateway on it until ctx is
// cancelled or the link drops. (Accept-one for now; thermal/fork re-attach is a
// follow-on.) Closing the listener on ctx.Done unblocks a pending Accept.
func serve(ctx context.Context, ln net.Listener, cfg gwConfig) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	conn, err := ln.Accept()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	gw, err := NewGateway(gateway.NewFrameConn(conn), cfg.ip, cfg.prefix, cfg.mac)
	if err != nil {
		conn.Close()
		return err
	}
	return gw.Run(ctx)
}
