//go:build !krucible

// This stub keeps `go build ./...` green on hosts without libkrun. The real
// helper (main.go) requires cgo + libkrun and is built with `-tags krucible`
// (see `make vmm`).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "bhatti-vmm was built without krucible support; build with `make vmm`")
	os.Exit(1)
}
