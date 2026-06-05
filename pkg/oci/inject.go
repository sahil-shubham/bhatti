package oci

import (
	"os"
	"path/filepath"
	"strings"
)

// injectLohar copies the lohar binary into the image tree and ensures
// the boot directories and uid 1000 user exist.
func injectLohar(rootDir, loharPath string) error {
	// Copy lohar binary
	dst := filepath.Join(rootDir, "usr/local/bin/lohar")
	os.MkdirAll(filepath.Dir(dst), 0755)
	if err := copyFile(loharPath, dst); err != nil {
		return err
	}
	os.Chmod(dst, 0755)

	// Ensure boot directories exist (lohar mounts these)
	for _, dir := range []string{
		"proc", "sys", "dev", "dev/pts", "tmp", "run", "workspace",
	} {
		os.MkdirAll(filepath.Join(rootDir, dir), 0755)
	}

	// Fix resolv.conf (may be a broken symlink from systemd-resolved)
	resolvPath := filepath.Join(rootDir, "etc/resolv.conf")
	os.Remove(resolvPath) // remove symlink if exists
	os.MkdirAll(filepath.Join(rootDir, "etc"), 0755)
	os.WriteFile(resolvPath, []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n"), 0644)

	// Ensure uid 1000 user exists
	return ensureUser1000(rootDir)
}

// ensureUser1000 checks if uid 1000 exists in /etc/passwd.
// If not, creates a 'lohar' user with uid 1000.
// If uid 1000 exists (e.g., 'node' in node images), leaves it as-is.
func ensureUser1000(rootDir string) error {
	passwdPath := filepath.Join(rootDir, "etc/passwd")
	data, err := os.ReadFile(passwdPath)
	if err != nil {
		return nil // no passwd file (scratch/distroless), skip
	}

	// Check if uid 1000 already exists
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) >= 4 && fields[2] == "1000" {
			// uid 1000 exists — ensure home directory exists
			homeDir := "/home/lohar"
			if len(fields) >= 6 && fields[5] != "" {
				homeDir = fields[5]
			}
			os.MkdirAll(filepath.Join(rootDir, homeDir), 0755)
			os.Chown(filepath.Join(rootDir, homeDir), 1000, 1000)
			return nil
		}
	}

	// uid 1000 doesn't exist — create manually
	f, err := os.OpenFile(passwdPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	f.WriteString("lohar:x:1000:1000::/home/lohar:/bin/sh\n")
	f.Close()

	// Add group entry if gid 1000 doesn't exist
	groupPath := filepath.Join(rootDir, "etc/group")
	groupData, _ := os.ReadFile(groupPath)
	gid1000Exists := false
	for _, line := range strings.Split(string(groupData), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) >= 3 && fields[2] == "1000" {
			gid1000Exists = true
			break
		}
	}
	if !gid1000Exists {
		if g, err := os.OpenFile(groupPath, os.O_APPEND|os.O_WRONLY, 0644); err == nil {
			g.WriteString("lohar:x:1000:\n")
			g.Close()
		}
	}

	// Create shadow entry (required for sudo on Debian/Ubuntu)
	shadowPath := filepath.Join(rootDir, "etc/shadow")
	if _, err := os.Stat(shadowPath); err == nil {
		if s, err := os.OpenFile(shadowPath, os.O_APPEND|os.O_WRONLY, 0640); err == nil {
			s.WriteString("lohar:!:19000:0:99999:7:::\n")
			s.Close()
		}
	}

	os.MkdirAll(filepath.Join(rootDir, "home/lohar"), 0755)
	os.Chown(filepath.Join(rootDir, "home/lohar"), 1000, 1000)
	return nil
}
