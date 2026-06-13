// Throwaway S0 probe: dial lohar through the krucible vsock bridge UDS.
// Diagnostic order: (1) raw UDS connect, (2) Activity (handled internally by
// lohar — needs no guest binary), (3) Exec (needs a binary in the rootfs).
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: krucible-probe <control-uds>")
		os.Exit(2)
	}
	uds := os.Args[1]

	// (1) raw connect — does the bridge accept + stay open?
	if conn, err := net.DialTimeout("unix", uds, 3*time.Second); err != nil {
		fmt.Println("raw dial FAILED:", err)
	} else {
		fmt.Println("raw dial OK (bridge accepted connection)")
		conn.Close()
	}

	c := agent.NewTestClient(uds, uds) // empty token => no auth

	// (2) Activity — internal to lohar, no guest binary required.
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		info, err := c.Activity(ctx)
		cancel()
		if err == nil {
			fmt.Printf("Activity OK: %+v\n", info)
			break
		}
		fmt.Printf("Activity attempt %d: %v\n", i+1, err)
		time.Sleep(500 * time.Millisecond)
	}

	// (3) Exec — needs /bin/true (busybox) in the rootfs.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if res, err := c.Exec(ctx, []string{"true"}, nil, ""); err != nil {
		fmt.Println("Exec(true) FAILED (expected if no busybox):", err)
	} else {
		fmt.Printf("Exec(true) OK: exit=%d\n", res.ExitCode)
	}
}
