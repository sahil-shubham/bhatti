package engine

// Cgroup placement modes, as an engine reports them.
const (
	// CgroupSystemd: every helper runs in a transient systemd scope of its own,
	// under bhatti.slice, so the kernel accounts CPU, memory and I/O per
	// sandbox and per owner.
	CgroupSystemd = "systemd"
	// CgroupNone: helpers are not placed; their usage lands in the daemon's own
	// cgroup, as it always has.
	CgroupNone = "none"
	// CgroupProcess: no cgroups on this platform (macOS); usage can only be
	// observed per process. Nothing is placed.
	CgroupProcess = "process"
)

// CgroupStatus is how an engine places its helpers, as the API reports it.
type CgroupStatus struct {
	Mode      string `json:"mode"`             // CgroupSystemd, CgroupNone or CgroupProcess
	Requested string `json:"requested"`        // what the config asked for: "auto", "systemd" or "none"
	Reason    string `json:"reason,omitempty"` // why an automatic choice settled on Mode, when it isn't systemd
}

// CgroupReporter is optionally implemented by engines that can place helpers
// in cgroups; the server reports its status on /health.
type CgroupReporter interface {
	CgroupStatus() CgroupStatus
}
