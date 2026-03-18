package proto

// ExecRequest is sent from the host to the guest agent to execute a command.
type ExecRequest struct {
	Argv       []string          `json:"argv"`
	Env        map[string]string `json:"env,omitempty"`
	TTY        *bool             `json:"tty,omitempty"`            // nil = false
	Rows       *uint16           `json:"rows,omitempty"`           // only used when TTY=true, default 24
	Cols       *uint16           `json:"cols,omitempty"`           // only used when TTY=true, default 80
	Cwd        *string           `json:"cwd,omitempty"`            // nil = agent's cwd (/)
	SessionID  *string           `json:"session_id,omitempty"`     // nil = create new, non-nil = attach
	MaxIdleSec *int              `json:"max_idle_sec,omitempty"`   // nil = default (0 = forever)
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
