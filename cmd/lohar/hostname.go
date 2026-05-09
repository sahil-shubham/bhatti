//go:build linux

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// applyHostname sets the kernel hostname and writes every on-disk surface
// that should reflect it: /etc/hostname, /etc/hosts, /etc/mailname.
//
// All four must agree. The base rootfs is built by debootstrap, which
// seeds /etc/hostname (and any package-installed /etc/mailname) from the
// build host's hostname. Without these per-boot rewrites, programs that
// read the file (hostnamectl, hostname --fqdn, mail tools) see the build
// runner's hostname — e.g. "runnervmeorf1" leaked from a GHA ARM64
// runner — while programs that read the syscall (the bash prompt, most
// daemons) see the correct create-time name. Reported in #16.
func applyHostname(hostname string) {
	syscall.Sethostname([]byte(hostname))
	writeHostnameFiles("/etc", hostname)
}

// writeHostnameFiles writes the on-disk hostname surfaces under etcRoot.
// Factored out from applyHostname so tests can verify file contents in a
// temp directory without calling sethostname(2), which would change the
// test runner's actual hostname.
func writeHostnameFiles(etcRoot, hostname string) {
	os.WriteFile(filepath.Join(etcRoot, "hostname"),
		[]byte(hostname+"\n"), 0644)
	os.WriteFile(filepath.Join(etcRoot, "hosts"), []byte(
		"127.0.0.1 localhost "+hostname+"\n"+
			"::1 localhost "+hostname+"\n"), 0644)
	os.WriteFile(filepath.Join(etcRoot, "mailname"),
		[]byte(hostname+"\n"), 0644)
}
