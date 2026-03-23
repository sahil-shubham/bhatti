package oci

import (
	"os"
	"path/filepath"
)

// validateImage checks for known incompatibilities and returns warnings.
func validateImage(rootDir string) []string {
	var warnings []string

	if exists(rootDir, "lib/systemd/systemd") || exists(rootDir, "usr/lib/systemd/systemd") {
		warnings = append(warnings, "image contains systemd — it will NOT run as PID 1, lohar replaces it")
	}

	if exists(rootDir, "usr/bin/dockerd") {
		warnings = append(warnings, "image contains dockerd — Docker-in-Docker is not supported in Firecracker VMs")
	}

	if exists(rootDir, "usr/local/cuda") {
		warnings = append(warnings, "image contains CUDA libraries — GPU passthrough is not supported in Firecracker")
	}

	hasShell := exists(rootDir, "bin/sh") || exists(rootDir, "usr/bin/sh") ||
		exists(rootDir, "bin/bash") || exists(rootDir, "usr/bin/bash")
	if !hasShell {
		warnings = append(warnings, "image has no /bin/sh — exec commands will fail")
	}

	if exists(rootDir, "usr/bin/fusermount") || exists(rootDir, "usr/bin/fusermount3") {
		warnings = append(warnings, "image contains FUSE tools — FUSE is not supported in the Firecracker guest kernel")
	}

	hasSudo := exists(rootDir, "usr/bin/sudo") || exists(rootDir, "bin/sudo")
	if !hasSudo {
		warnings = append(warnings, "image does not have sudo — commands that need root will fail")
	}

	return warnings
}

func exists(rootDir, relPath string) bool {
	_, err := os.Stat(filepath.Join(rootDir, relPath))
	return err == nil
}
