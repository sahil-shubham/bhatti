package engine

import (
	"context"
	"io"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
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

// PersistentVolume describes a named volume to attach to a sandbox.
// Used in the API request → server layer volume resolution.
type PersistentVolume struct {
	Name       string `json:"name"`        // volume name (scoped to user)
	Mount      string `json:"mount"`       // mount point inside VM
	SizeMB     int    `json:"size_mb"`     // used only if AutoCreate
	AutoCreate bool   `json:"auto_create"` // create if doesn't exist
	ReadOnly   bool   `json:"read_only"`
}

// ResolvedVolume is a fully resolved volume reference with host file path.
// The server layer handles name resolution, auto-create, and attachment.
// The engine layer receives only these resolved volumes.
type ResolvedVolume struct {
	FilePath string `json:"file_path"` // host path to ext4 file
	DriveID  string `json:"drive_id"`  // Firecracker drive ID ("vol0")
	Name     string `json:"name"`      // volume name
	Mount    string `json:"mount"`     // guest mount point
	ReadOnly bool   `json:"read_only"`
}

// FileSpec describes a file to inject into the sandbox.
type FileSpec struct {
	Content []byte `json:"content"` // raw content (will be base64-encoded in config drive)
	Mode    string `json:"mode"`    // e.g. "0600"
}

// SandboxSpec describes what to create.
type SandboxSpec struct {
	Name       string              `json:"name"`
	Image      string              `json:"image"`
	CPUs       float64             `json:"cpus"`
	MemoryMB   int                 `json:"memory_mb"`
	DiskSizeMB int                 `json:"disk_size_mb"`
	Env        map[string]string   `json:"env"`
	Labels     map[string]string   `json:"labels,omitempty"`   // deprecated: only used by template path
	UserData   string              `json:"userdata,omitempty"` // deprecated: only used by template path
	Volumes    []VolumeMount       `json:"volumes,omitempty"`
	Mounts     []FsMount           `json:"mounts,omitempty"` // live virtio-fs host-dir binds (create --mount)
	Secrets    []SecretRef         `json:"secrets,omitempty"`
	Files      map[string]FileSpec `json:"files,omitempty"` // path → content
	NewVolumes []VolumeSpec        `json:"new_volumes,omitempty"`
	Init       string              `json:"init,omitempty"`
	Hugepages  bool                `json:"hugepages,omitempty"` // 2MB hugepages, faster boot, no Diff snapshots

	// v0.3: Persistent volume references (replaces VolumeMount for persistent vols)
	PersistentVolumes []PersistentVolume `json:"persistent_volumes,omitempty"`

	// Set by server layer, not by API clients
	UserID          string           `json:"-"` // owner's user ID
	SubnetIndex     int              `json:"-"` // owner's subnet index for network isolation
	BaseImage       string           `json:"-"` // resolved image file path
	ResolvedVolumes []ResolvedVolume `json:"-"` // resolved volume file paths
}

// FsMount is a live virtio-fs host-directory bind (create --mount): the host
// directory is exposed to the guest at GuestPath — shared, bidirectional, and
// N-writer (the host FS arbitrates). Distinct from a volume (an owned, versioned,
// portable block disk). FC ignores this; krucible wires it via krun_add_virtiofs3
// (host side) + a guest virtio-fs mount (lohar, from the config drive).
type FsMount struct {
	HostPath  string `json:"host_path"`
	GuestPath string `json:"guest_path"`
	ReadOnly  bool   `json:"read_only,omitempty"`
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

// StreamEvent is emitted during streaming exec.
type StreamEvent struct {
	Type     string `json:"type"`                // "stdout", "stderr", "exit", "error"
	Data     string `json:"data,omitempty"`      // output text
	ExitCode *int   `json:"exit_code,omitempty"` // only for type="exit"
}

// StreamExecEngine is optionally implemented by engines that support
// streaming exec output. Used by the NDJSON exec endpoint.
type StreamExecEngine interface {
	ExecStream(ctx context.Context, id string, cmd []string, onEvent func(StreamEvent)) error
}

// DetachedExecEngine is optionally implemented by engines that support
// fire-and-forget command execution. The command runs in its own session
// (setsid) and survives vsock connection close. Returns the PID and the
// output file path.
type DetachedExecEngine interface {
	ExecDetached(ctx context.Context, id string, cmd []string, outputFile string) (pid int, outputPath string, err error)
}

// SessionAttacher is optionally implemented by engines that support
// reconnecting to existing TTY sessions.
//
// ifDetached: if true, attach only if the session is currently detached.
// Returns an error if the session is attached by another client. This
// prevents the auto-reattach TOCTOU race (SessionList says "detached",
// but another client attached between list and attach).
type SessionAttacher interface {
	ShellAttach(ctx context.Context, id, sessionID string, ifDetached bool) (*proto.SessionInfo, TerminalConn, error)
}

// ShellSessioner is optionally implemented by engines that return
// session metadata alongside the terminal connection.
type ShellSessioner interface {
	ShellSession(ctx context.Context, id string) (string, TerminalConn, error)
}

// PipedConn is a bidirectional frame-level connection for piped sessions.
// Unlike TerminalConn (byte-stream), PipedConn exposes the underlying
// frame protocol so callers can distinguish STDOUT, EXIT, and ERROR frames.
type PipedConn interface {
	ReadFrame() (msgType byte, payload []byte, err error)
	WriteStdin(data []byte) error
	Kill() error
	Close() error
}

// PipedSessionEngine is optionally implemented by engines that support
// non-TTY persistent sessions with scrollback and reattach.
type PipedSessionEngine interface {
	PipedSession(ctx context.Context, id string, cmd []string,
		env map[string]string, maxIdleSec int) (*proto.SessionInfo, PipedConn, error)
	PipedSessionAttach(ctx context.Context, id, sessionID string,
		ifDetached bool) (*proto.SessionInfo, PipedConn, error)
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
