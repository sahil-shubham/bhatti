package engine

// What an engine's usage figures cover, per sandbox. These are the values
// Engine.Accounting reports and the collector passes on.
const (
	// AccountingCgroup: the VM's helper has run in its own cgroup since it
	// began, so every figure covers all of what the VM did.
	AccountingCgroup = "cgroup"
	// AccountingPartial: the helper was placed in its cgroup after it had
	// started; what it used before the move is charged to the daemon's cgroup.
	AccountingPartial = "partial"
	// AccountingProcess: no cgroups here (macOS); the figures are per process.
	AccountingProcess = "process"
	// AccountingNone: the sandbox's use isn't measured (no cgroup placement, or
	// the helper isn't in the cgroup it should be).
	AccountingNone = "none"
)

// StatsSource is optionally implemented by engines whose sandboxes can be
// measured. The collector samples what it lists, every interval.
type StatsSource interface {
	// StatsTargets says what exists and where to read it. It is a snapshot,
	// cheap, and never waits for a sandbox or gateway to finish starting or
	// stopping: a gateway mid-transition is left out of this snapshot.
	StatsTargets() StatsTargets
}

// StatsTargets is everything an engine can be measured by.
type StatsTargets struct {
	// Sandboxes lists every sandbox the engine knows, running or not: the
	// collector keeps a stopped sandbox's last hour until it is destroyed.
	Sandboxes []StatsSandbox
	// Owners lists the owners that have at least one measured cgroup.
	Owners []StatsOwner
	// Netds lists the running network gateways, one per owner.
	Netds []StatsNetd
}

// StatsSandbox is one sandbox's VM.
type StatsSandbox struct {
	ID         string // the engine's id for the sandbox: the one in its scope's name
	Owner      string // the owner's slug, which names its slice and its gateway
	UserID     string // the user that owns the sandbox; "" for one with no owner
	PID        int    // the VM helper's pid; 0 when the VM isn't running
	Accounting string // Accounting*: what a sample of this sandbox covers
	// Cgroup is the path of the sandbox's scope below the cgroup v2 mount, and
	// is only set when Accounting is AccountingCgroup or AccountingPartial and
	// the helper is in that cgroup now: a helper that isn't there is never
	// measured by whatever cgroup it is in instead.
	Cgroup string
	NetIP  string // the guest's address on its owner's gateway, which is how the gateway's per-guest figures are matched to it; "" without a NIC
}

// StatsOwner is an owner's slice, which the kernel keeps the sum of its
// sandboxes' and its gateway's use in.
type StatsOwner struct {
	Owner  string // the owner's slug
	UserID string // the owner's user; "" for a sandbox that is its own owner
	Cgroup string // the slice's path below the cgroup v2 mount
}

// StatsNetd is one owner's gateway process.
type StatsNetd struct {
	Owner      string // the owner's slug
	PID        int    // the gateway's pid
	Accounting string // Accounting*: what a sample of the gateway's own use covers
	Cgroup     string // as StatsSandbox.Cgroup, for the gateway's scope
	CtlSock    string // the gateway's control socket, which answers the stats request
}
