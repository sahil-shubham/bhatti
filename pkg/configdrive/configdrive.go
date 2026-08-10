// Package configdrive defines the bhatti sandbox config schema: the JSON
// (hostname, auth token, env, files, volumes, DNS, init, net) lohar fetches over
// the guest→host config vsock at boot (DESIGN-bhatti-v2-secrets-and-trust §3.4),
// before the agent starts listening. It replaced the on-disk ext4 "config drive"
// (retired along with mke2fs); the package name is kept for continuity. These
// field names are the wire contract with lohar's reader (cmd/lohar/main.go:
// SandboxConfig) — keep them in sync.
package configdrive

// SandboxConfig is the JSON lohar fetches over the config vsock at boot. Field
// names are the wire contract with cmd/lohar/main.go.
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
	// Net, if set, tells lohar to configure eth0 (virtio-net gateway path) from
	// the config drive via netlink — no `ip` binary / kernel IP autoconfig needed.
	Net *NetConfig `json:"net,omitempty"`
}

// NetConfig is the guest's eth0 addressing on the per-owner gateway network.
type NetConfig struct {
	IP      string `json:"ip"`      // CIDR, e.g. "100.64.0.2/24"
	Gateway string `json:"gateway"` // e.g. "100.64.0.1"
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
