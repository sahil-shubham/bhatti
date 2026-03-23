//go:build linux

package firecracker

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// SandboxConfig is the JSON structure written to the config drive.
// Lohar reads this at boot to configure the VM.
type SandboxConfig struct {
	SandboxID string            `json:"sandbox_id"`
	Hostname  string            `json:"hostname"`
	Token     string            `json:"token"`
	Env       map[string]string `json:"env"`
	Files     map[string]ConfigFile `json:"files"`
	Volumes   []VolumeMountConfig   `json:"volumes"`
	Init      string            `json:"init,omitempty"`
	DNS       []string          `json:"dns"`
	User      string            `json:"user"`
}

// ConfigFile represents a file to write into the guest filesystem.
type ConfigFile struct {
	Content string `json:"content"` // base64-encoded
	Mode    string `json:"mode"`    // e.g. "0600"
}

// VolumeMountConfig maps a block device to a mount point inside the guest.
// This type is the contract between the engine (host) and lohar (guest) via
// the config drive JSON. Both sides must agree on the field names.
type VolumeMountConfig struct {
	Device   string `json:"device"`    // e.g. "/dev/vdc"
	Mount    string `json:"mount"`     // e.g. "/workspace"
	FS       string `json:"fs"`        // e.g. "ext4"
	ReadOnly bool   `json:"read_only"` // mount with MS_RDONLY in guest
}

// createConfigDrive builds an ext4 image containing config.json using mke2fs -d.
// This avoids mount/umount (no leaked loop devices on crash) and supports
// larger payloads (up to 4MB) for sandboxes with many volumes/env vars.
func createConfigDrive(path string, cfg SandboxConfig) error {
	// 1. Marshal config to a temp directory
	tmpDir, err := os.MkdirTemp("", "bhatti-config-*")
	if err != nil {
		return fmt.Errorf("create config temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), data, 0644); err != nil {
		return fmt.Errorf("write config.json: %w", err)
	}

	// 2. Size: config JSON + headroom, minimum 1MB, max 4MB
	sizeMB := len(data)*3/2/1024/1024 + 1
	if sizeMB < 1 {
		sizeMB = 1
	}
	if sizeMB > 4 {
		sizeMB = 4
	}

	// 3. Create sparse file
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create config drive: %w", err)
	}
	if err := f.Truncate(int64(sizeMB) << 20); err != nil {
		f.Close()
		return fmt.Errorf("truncate config drive: %w", err)
	}
	f.Close()

	// 4. Format and populate in one step using mke2fs -d (no mount needed)
	if out, err := exec.Command("mke2fs", "-t", "ext4", "-d", tmpDir,
		"-F", "-q", path).CombinedOutput(); err != nil {
		os.Remove(path)
		return fmt.Errorf("mke2fs config drive: %s: %w", out, err)
	}
	return nil
}

// createVolume creates an ext4 image of the specified size in MB.
func createVolume(path string, sizeMB int) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create volume: %w", err)
	}
	if err := f.Truncate(int64(sizeMB) << 20); err != nil {
		f.Close()
		return fmt.Errorf("truncate volume: %w", err)
	}
	f.Close()
	if err := exec.Command("mkfs.ext4", "-F", "-q", path).Run(); err != nil {
		return fmt.Errorf("mkfs volume: %w", err)
	}
	return nil
}
