// Command krucible-mkimage builds a krucible block-root base image from an OCI
// reference (e.g. "alpine", "ubuntu:24.04") via oci.PullAndConvert, injecting
// the lohar agent at /init.krun. It's a dev/bench helper: in production the
// server pulls and converts images itself. Pure Go (no cgo) — needs `mke2fs`
// (e2fsprogs) and network access to the registry.
//
// Usage: krucible-mkimage <oci-ref> <out.img> [lohar-path]
//
//	lohar-path defaults to ./lohar; it must be a linux/<host-arch> binary
//	(guest arch == host arch under HVF/KVM).
package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/oci"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: krucible-mkimage <oci-ref> <out.img> [lohar-path]")
		os.Exit(2)
	}
	ref, out := os.Args[1], os.Args[2]
	lohar := "./lohar"
	if len(os.Args) > 3 {
		lohar = os.Args[3]
	}
	if _, err := os.Stat(lohar); err != nil {
		fmt.Fprintf(os.Stderr, "krucible-mkimage: lohar binary not found at %q (cross-build with GOOS=linux GOARCH=%s): %v\n", lohar, runtime.GOARCH, err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cfg, err := oci.PullAndConvert(ctx, ref, out, lohar,
		oci.WithPlatform("linux", runtime.GOARCH),
		oci.WithProgress(func(s string) { fmt.Fprintln(os.Stderr, "  "+s) }),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "krucible-mkimage: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("built %s from %s (linux/%s)\n", out, ref, runtime.GOARCH)
	if cfg != nil {
		fmt.Printf("  cmd=%v workdir=%q user=%q size=%dMB\n", cfg.Cmd, cfg.WorkingDir, cfg.User, cfg.TotalSize>>20)
	}
}
