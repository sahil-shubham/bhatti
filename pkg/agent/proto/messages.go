package proto

// AgentFeature names behavior that may be missing from a sandbox's baked-in
// lohar, even when the host daemon has already been upgraded.
type AgentFeature string

const (
	FeatureNetConfigMAC AgentFeature = "net_config_mac"
	FeatureSandboxCA    AgentFeature = "sandbox_ca"
	FeatureRootGrowth   AgentFeature = "root_growth"
	FeaturePipedStderr  AgentFeature = "piped_stderr"
	FeatureReseedCRNG   AgentFeature = "reseed_crng"
)

// AgentInfo is returned by INFO_RESP. Legacy is host-only: an old lohar
// explicitly rejected INFO_REQ, rather than a query failing for another reason.
type AgentInfo struct {
	Version  string         `json:"version"`
	Features []AgentFeature `json:"features"`
	Legacy   bool           `json:"-"`
}

func (i AgentInfo) Has(feature AgentFeature) bool {
	for _, f := range i.Features {
		if f == feature {
			return true
		}
	}
	return false
}

// ExecRequest is sent from the host to the guest agent to execute a command.
type ExecRequest struct {
	Argv       []string          `json:"argv"`
	Env        map[string]string `json:"env,omitempty"`
	TTY        *bool             `json:"tty,omitempty"`          // nil = false
	Rows       *uint16           `json:"rows,omitempty"`         // only used when TTY=true, default 24
	Cols       *uint16           `json:"cols,omitempty"`         // only used when TTY=true, default 80
	Cwd        *string           `json:"cwd,omitempty"`          // nil = agent's cwd (/)
	SessionID  *string           `json:"session_id,omitempty"`   // nil = create new, non-nil = attach
	MaxIdleSec *int              `json:"max_idle_sec,omitempty"` // nil = default (0 = forever)
	IfDetached *bool             `json:"if_detached,omitempty"`  // attach only if session is detached
	Detach     *bool             `json:"detach,omitempty"`       // fire-and-forget: start and return PID immediately
	OutputFile *string           `json:"output_file,omitempty"`  // detach: redirect stdout/stderr to this file
	Session    *bool             `json:"session,omitempty"`      // non-TTY session with scrollback+reattach (piped)
	Stderr     *bool             `json:"stderr,omitempty"`       // piped session: send stderr as STDERR frames instead of merging it into stdout
}

// ActivityInfo reports the agent's activity state.
type ActivityInfo struct {
	LastActivityUnix int64 `json:"last_activity_unix"`
	ActiveSessions   int   `json:"active_sessions"`   // running processes
	AttachedSessions int   `json:"attached_sessions"` // connected clients
}

// SessionInfo describes a running or completed session.
type SessionInfo struct {
	SessionID string `json:"session_id"`
	Argv      string `json:"argv"`
	TTY       bool   `json:"tty"`
	Running   bool   `json:"running"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Attached  bool   `json:"attached"`
	CreatedAt int64  `json:"created_at"` // unix timestamp
}

// ForwardRequest is sent from the host to the guest to open a TCP tunnel.
type ForwardRequest struct {
	Port uint16 `json:"port"`
}

// ForwardResponse is sent from the guest back to the host after a ForwardRequest.
type ForwardResponse struct {
	Status  string  `json:"status"`            // "ok" or "error"
	Message *string `json:"message,omitempty"` // error detail when Status="error"
}

// FileInfo describes a file in the guest filesystem.
type FileInfo struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Mode  string `json:"mode"` // octal string, e.g. "0644"
	IsDir bool   `json:"is_dir"`
	Mtime int64  `json:"mtime"` // unix timestamp
}

// SystemctlRequest is the body of a SYSTEMCTL_REQ frame: a single privileged
// unit operation (start/stop/restart/enable/etc.) and its arguments. The
// caller's identity is NOT carried in this struct — the server uses
// SO_PEERCRED on the Unix socket to learn the caller's uid authoritatively.
// Trusting a client-claimed uid would be a security bug.
type SystemctlRequest struct {
	Op    string            `json:"op"`
	Units []string          `json:"units"`
	Flags map[string]string `json:"flags,omitempty"`
}

// SystemctlResponse is the body of a SYSTEMCTL_RESP frame. The op has
// already been executed; this carries the result the client should print
// and exit with.
type SystemctlResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}
