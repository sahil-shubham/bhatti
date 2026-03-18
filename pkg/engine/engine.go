package engine

import (
	"context"
	"io"
)

// VolumeMount describes a named volume to mount into a sandbox.
type VolumeMount struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readonly,omitempty"`
}

// SecretRef references a secret to inject into the sandbox.
type SecretRef struct {
	Name string `json:"name"` // secret name in store
	Path string `json:"path"` // file path inside guest OR env var name
	Mode string `json:"mode"` // file mode (e.g. "0600"); empty = inject as env var
}

// VolumeSpec describes a volume to create and attach to the sandbox.
type VolumeSpec struct {
	Name   string `json:"name"`
	SizeMB int    `json:"size_mb"`
	Mount  string `json:"mount"`
}

// SandboxSpec describes what to create.
type SandboxSpec struct {
	Name       string            `json:"name"`
	Image      string            `json:"image"`
	CPUs       float64           `json:"cpus"`
	MemoryMB   int               `json:"memory_mb"`
	DiskSizeMB int               `json:"disk_size_mb"`
	Env        map[string]string `json:"env"`
	Labels     map[string]string `json:"labels"`
	UserData   string            `json:"userdata"`
	Volumes    []VolumeMount     `json:"volumes,omitempty"`
	Secrets    []SecretRef       `json:"secrets,omitempty"`
	NewVolumes []VolumeSpec      `json:"new_volumes,omitempty"`
	Init       string            `json:"init,omitempty"`
}

// SandboxInfo is the runtime state of a sandbox.
type SandboxInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"` // "running", "stopped", "unknown"
	IP       string `json:"ip"`
	EngineID string `json:"engine_id"`
}

// ExecResult holds the output of a command execution.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// TerminalConn is a bidirectional TTY connection.
type TerminalConn interface {
	io.ReadWriteCloser
	Resize(rows, cols int) error
}

// VMStateProvider is optionally implemented by engines that persist VM state.
type VMStateProvider interface {
	VMState(id string) map[string]interface{}
	RestoreVM(id, name, status string, state map[string]interface{})
}

// Engine is the sandbox lifecycle interface.
type Engine interface {
	Create(ctx context.Context, spec SandboxSpec) (SandboxInfo, error)
	Destroy(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Start(ctx context.Context, id string) error
	Status(ctx context.Context, id string) (SandboxInfo, error)
	List(ctx context.Context) ([]SandboxInfo, error)
	Exec(ctx context.Context, id string, cmd []string) (ExecResult, error)
	Shell(ctx context.Context, id string) (TerminalConn, error)
	ListeningPorts(ctx context.Context, id string) ([]int, error)

	// Tunnel opens a bidirectional byte stream to localhost:port inside the
	// sandbox. The caller reads/writes raw TCP bytes. How the connection is
	// established is engine-specific (Docker: exec socat, Firecracker: vsock
	// to guest agent). The returned connection must be closed by the caller.
	Tunnel(ctx context.Context, id string, port int) (io.ReadWriteCloser, error)
}
