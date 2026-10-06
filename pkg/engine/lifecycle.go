package engine

// LifecycleKind names a change detected by the engine rather than an API request.
type LifecycleKind string

const (
	VMExited    LifecycleKind = "vm_exited"
	NetworkLost LifecycleKind = "network_lost"
)

// LifecycleEvent describes a change the engine observed on its own.
type LifecycleEvent struct {
	Kind      LifecycleKind
	EngineID  string // the engine's VM ID
	SandboxID string // the server sandbox ID, when known
	UserID    string // owner, when known
	ExitCode  int    // VMExited: normal exit status; -1 if unknown or signaled
	Signal    string // VMExited: signal name, if signaled
	Reason    string // human-readable detail
}

// LifecycleReporter is implemented by engines that detect lifecycle changes.
// Events observed during startup recovery are queued until a handler is set.
// The handler must not block for long.
type LifecycleReporter interface {
	SetLifecycleHandler(func(LifecycleEvent))
}
