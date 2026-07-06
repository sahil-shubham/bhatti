package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os/signal"
	"syscall"

	"github.com/sahil-shubham/bhatti/pkg/gateway"

	"gvisor.dev/gvisor/pkg/tcpip"
)

// bhatti-netd --net-uds <path> --gw-ip 100.64.0.1 --prefix 24 --mac 52:54:00:00:00:01
//
// Connects to the libkrun virtio-net unixstream UDS and runs the gateway. The
// daemon spawns one per owner and wires the UDS into the VM via
// krun_add_net_unixstream. Egress policy / substitution / inbound are layered on
// in later steps.
func main() {
	netUDS := flag.String("net-uds", "", "libkrun virtio-net unixstream socket path")
	gwIP := flag.String("gw-ip", "100.64.0.1", "gateway IPv4 address")
	prefix := flag.Int("prefix", 24, "gateway subnet prefix length")
	macStr := flag.String("mac", "52:54:00:00:00:01", "gateway link (MAC) address")
	flag.Parse()

	if *netUDS == "" {
		log.Fatal("bhatti-netd: --net-uds is required")
	}
	mac, err := net.ParseMAC(*macStr)
	if err != nil {
		log.Fatalf("bhatti-netd: bad --mac %q: %v", *macStr, err)
	}
	ip := net.ParseIP(*gwIP).To4()
	if ip == nil {
		log.Fatalf("bhatti-netd: bad --gw-ip %q", *gwIP)
	}

	conn, err := net.Dial("unix", *netUDS)
	if err != nil {
		log.Fatalf("bhatti-netd: dial %s: %v", *netUDS, err)
	}
	defer conn.Close()

	gw, err := NewGateway(
		gateway.NewFrameConn(conn),
		tcpip.AddrFrom4([4]byte{ip[0], ip[1], ip[2], ip[3]}),
		*prefix,
		tcpip.LinkAddress(mac),
	)
	if err != nil {
		log.Fatalf("bhatti-netd: gateway: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	log.Printf("bhatti-netd: gateway up on %s (gw %s/%d, mac %s)", *netUDS, *gwIP, *prefix, *macStr)
	if err := gw.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("bhatti-netd: %v", err)
	}
}
