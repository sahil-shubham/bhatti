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
type VolumeMountConfig struct {
	Device string `json:"device"` // e.g. "/dev/vdc"
	Mount  string `json:"mount"`  // e.g. "/workspace"
	FS     string `json:"fs"`     // e.g. "ext4"
}

// createConfigDrive builds a 1MB ext4 image containing config.json.
func createConfigDrive(path string, cfg SandboxConfig) error {
	// 1. Create 1MB sparse file
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create config drive: %w", err)
	}
	if err := f.Truncate(1 << 20); err != nil {
		f.Close()
		return fmt.Errorf("truncate config drive: %w", err)
	}
	f.Close()

	// 2. Format as ext4
	if err := exec.Command("mkfs.ext4", "-F", "-q", path).Run(); err != nil {
		return fmt.Errorf("mkfs config drive: %w", err)
	}

	// 3. Mount, write config.json, unmount
	mountDir := path + ".mnt"
	os.MkdirAll(mountDir, 0700)
	if err := exec.Command("mount", path, mountDir).Run(); err != nil {
		os.Remove(mountDir)
		return fmt.Errorf("mount config drive: %w", err)
	}
	defer func() {
		exec.Command("umount", mountDir).Run()
		os.Remove(mountDir)
	}()

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(mountDir, "config.json"), data, 0644); err != nil {
		return fmt.Errorf("write config.json: %w", err)
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
