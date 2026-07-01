// Package configdrive builds the bhatti config drive: a small ext4 image
// carrying config.json (hostname, auth token, env, files, volumes, DNS, init)
// that lohar reads at /dev/vdb on boot, before the agent starts listening.
//
// This is the engine↔guest configuration contract. The SandboxConfig /
// ConfigFile / VolumeMountConfig field names MUST stay in sync with lohar's
// reader (cmd/lohar/main.go: SandboxConfig). It is cross-platform (mke2fs -d,
// no mount/loop, no OS-specific syscalls) so both the krucible engine (macOS +
// Linux) and the Firecracker engine can use it.
package configdrive

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// SandboxConfig is the JSON written to the config drive and read by lohar at
// boot. Field names are the wire contract with cmd/lohar/main.go.
type SandboxConfig struct {
	SandboxID   string                `json:"sandbox_id"`
	Hostname    string                `json:"hostname"`
	Token       string                `json:"token"`
	Env         map[string]string     `json:"env"`
	Files       map[string]ConfigFile `json:"files"`
	Volumes     []VolumeMountConfig   `json:"volumes"`
	Mounts      []FsMountConfig       `json:"mounts,omitempty"` // virtio-fs binds: tag → guest mount path
	Init        string                `json:"init,omitempty"`
	DNS         []string              `json:"dns"`
	DNSInternal string                `json:"dns_internal,omitempty"`
	User        string                `json:"user"`
}

// ConfigFile is a file to materialize in the guest filesystem at boot.
type ConfigFile struct {
	Content string `json:"content"` // base64-encoded
	Mode    string `json:"mode"`    // octal, e.g. "0600"
}

// VolumeMountConfig maps a guest block device to a mount point. Both host
// (writer) and lohar (reader) must agree on the field names.
type VolumeMountConfig struct {
	Device   string `json:"device"`    // e.g. "/dev/vdc"
	Mount    string `json:"mount"`     // e.g. "/workspace"
	FS       string `json:"fs"`        // e.g. "ext4"
	ReadOnly bool   `json:"read_only"` // mount MS_RDONLY in the guest
}

// FsMountConfig tells the guest (lohar) to mount a virtio-fs device (by Tag,
// matching the VMM's krun_add_virtiofs3) at Mount. The host directory lives on
// the VMM side; the guest only needs the tag + where to mount it.
type FsMountConfig struct {
	Tag      string `json:"tag"`
	Mount    string `json:"mount"`
	ReadOnly bool   `json:"read_only"`
}

// Build writes config.json into a fresh ext4 image at path via `mke2fs -d`
// (no mount/loop — no leaked loop devices, works unprivileged on macOS and
// Linux). The image is sized to the payload (1–4 MiB).
func Build(path string, cfg SandboxConfig) error {
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

	// Payload + headroom, clamped to [1, 4] MiB (matches the FC builder).
	sizeMB := len(data)*3/2/1024/1024 + 1
	if sizeMB > 4 {
		sizeMB = 4
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create config drive: %w", err)
	}
	if err := f.Truncate(int64(sizeMB) << 20); err != nil {
		f.Close()
		return fmt.Errorf("truncate config drive: %w", err)
	}
	f.Close()

	if out, err := exec.Command("mke2fs", "-t", "ext4", "-d", tmpDir, "-F", "-q", path).CombinedOutput(); err != nil {
		os.Remove(path)
		return fmt.Errorf("mke2fs config drive: %s: %w", out, err)
	}
	return nil
}
