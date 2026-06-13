#!/usr/bin/env bash
# Build a krucible base rootfs (virtiofs dir): lohar as /init.krun plus a tiny
# multi-call "box" util (true/false/echo/errcho/sleep/netcheck) so exec + egress
# demos work. This is a minimal dev base — a full Ubuntu/busybox userland (bash,
# ss, node, ...) is a separate pipeline. Guest arch == host arch.
#
# Usage: scripts/krucible-rootfs.sh [OUT_DIR]   (default: dist/krucible-rootfs)
set -euo pipefail
cd "$(dirname "$0")/.."
REPO="$(pwd)"
OUT="${1:-$REPO/dist/krucible-rootfs}"
ARCH="$(go env GOHOSTARCH)"

echo "==> building krucible base rootfs at $OUT (linux/$ARCH)"
rm -rf "$OUT"
mkdir -p "$OUT"/{bin,usr/local/bin,proc,sys,dev/pts,tmp,run,etc,root,workspace}

echo "    lohar -> /init.krun"
GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 go build -o "$OUT/init.krun" ./cmd/lohar

echo "    box (true/false/echo/errcho/sleep/netcheck) -> /bin"
TD="$(mktemp -d)"
cat > "$TD/go.mod" <<'EOF'
module box
go 1.21
EOF
cat > "$TD/main.go" <<'EOF'
package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	switch filepath.Base(os.Args[0]) {
	case "echo":
		fmt.Println(strings.Join(os.Args[1:], " "))
	case "errcho":
		fmt.Fprintln(os.Stderr, strings.Join(os.Args[1:], " "))
	case "false":
		os.Exit(1)
	case "sleep":
		if len(os.Args) > 1 {
			n, _ := strconv.Atoi(os.Args[1])
			time.Sleep(time.Duration(n) * time.Second)
		}
	case "netcheck":
		netcheck()
	default: // true
	}
}

func netcheck() {
	mode := "tcp"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "tcp":
		c, err := net.DialTimeout("tcp", "1.1.1.1:443", 5*time.Second)
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		c.Close()
		fmt.Println("OK tcp 1.1.1.1:443")
	case "dns":
		ips, err := net.LookupHost("example.com")
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		fmt.Println("OK dns", ips)
	case "http":
		cl := &http.Client{Timeout: 8 * time.Second}
		r, err := cl.Get("http://example.com")
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		r.Body.Close()
		fmt.Println("OK http", r.Status)
	case "serve":
		http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, "hello-from-guest\n")
		})
		http.ListenAndServe("0.0.0.0:"+os.Args[2], nil)
	}
}
EOF
( cd "$TD" && GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 go build -o "$OUT/bin/true" . )
rm -rf "$TD"
for n in false echo errcho sleep netcheck; do ln -sf true "$OUT/bin/$n"; done

echo "==> done. point krucible_rootfs at: $OUT"
ls -la "$OUT" "$OUT/bin"
