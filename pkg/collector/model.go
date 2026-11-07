// Package collector samples what every sandbox, every owner and the host use,
// keeps the latest figures and the last hour of each sandbox in memory, and
// renders them as Prometheus text. It reads: the cgroup v2 accounting files of
// each sandbox's scope and each owner's slice (Linux, where bhatti places its
// helpers in systemd scopes), the per-process figures of each VM helper
// (macOS, which has no cgroups), each owner's gateway through its control
// socket, and the host's /proc.
//
// Nothing here limits or acts on what it reads, and nothing is stored on disk:
// history beyond the hour is the job of whatever scrapes the Prometheus text.
package collector

import "time"

// CPU is a cgroup's cpu.stat: the CPU time all of its processes have used,
// cumulative, in microseconds.
type CPU struct {
	UsageUsec  uint64 `json:"usage_usec"`
	UserUsec   uint64 `json:"user_usec"`
	SystemUsec uint64 `json:"system_usec"`
}

// Memory is a cgroup's memory in bytes. Peak and the anon/file split are only
// there when the kernel reports them (memory.peak came in Linux 5.19), and a
// macOS process has only its resident size.
type Memory struct {
	Current uint64 `json:"current"`
	Peak    uint64 `json:"peak,omitempty"` // high-water mark since the cgroup was made
	Anon    uint64 `json:"anon,omitempty"`
	File    uint64 `json:"file,omitempty"`
	HasPeak bool   `json:"-"` // Peak was read
	HasStat bool   `json:"-"` // Anon and File were read
}

// IO is a cgroup's block I/O, cumulative, from io.stat. The figures are what
// the kernel counted at the block devices the cgroup's I/O reached, summed over
// them: a device that is built on others (md, dm) is left out, because the
// kernel counts the same I/O at it and at the devices beneath it. A macOS
// process reports bytes only.
type IO struct {
	ReadBytes  uint64 `json:"read_bytes"`
	WriteBytes uint64 `json:"write_bytes"`
	ReadOps    uint64 `json:"read_ops"`
	WriteOps   uint64 `json:"write_ops"`
	HasOps     bool   `json:"-"` // ReadOps and WriteOps are counted
}

// PSI is one resource's pressure stall time, cumulative in microseconds. Some
// is the time at least one task was stalled waiting for the resource, Full the
// time every non-idle task was.
type PSI struct {
	SomeUsec uint64 `json:"some_usec"`
	FullUsec uint64 `json:"full_usec,omitempty"`
	HasFull  bool   `json:"-"` // the file had a "full" line (cpu.pressure didn't before Linux 5.13)
}

// Pressure is a cgroup's, or the host's, pressure stall information. A nil
// resource is one the kernel doesn't report here (no PSI, controller off).
type Pressure struct {
	CPU    *PSI `json:"cpu,omitempty"`
	Memory *PSI `json:"memory,omitempty"`
	IO     *PSI `json:"io,omitempty"`
}

// MemEvents is a cgroup's memory.events: how often the kernel acted on its
// memory, cumulative. Without limits only OOM and OOMKill can move.
type MemEvents struct {
	Low          uint64 `json:"low"`
	High         uint64 `json:"high"`
	Max          uint64 `json:"max"`
	OOM          uint64 `json:"oom"`
	OOMKill      uint64 `json:"oom_kill"`
	OOMGroupKill uint64 `json:"oom_group_kill"`
}

// Usage is what one cgroup (or, on macOS, one process) has used, read at one
// instant. A nil group wasn't available there.
type Usage struct {
	CPU      *CPU       `json:"cpu,omitempty"`
	Memory   *Memory    `json:"memory,omitempty"`
	IO       *IO        `json:"io,omitempty"`
	Pressure *Pressure  `json:"pressure,omitempty"`
	Events   *MemEvents `json:"events,omitempty"`
}

// Net is one guest's traffic through its owner's gateway, cumulative, from the
// guest's side: Rx is what the guest received. Dropped counts what the gateway
// refused to carry.
type Net struct {
	RxBytes         uint64 `json:"rx_bytes"`
	RxPackets       uint64 `json:"rx_packets"`
	TxBytes         uint64 `json:"tx_bytes"`
	TxPackets       uint64 `json:"tx_packets"`
	DroppedPolicy   uint64 `json:"dropped_policy"`
	DroppedSpoof    uint64 `json:"dropped_spoof"`
	DroppedHardDeny uint64 `json:"dropped_hard_deny"`
}

// Rates are derived from two consecutive samples of the same thing. A counter
// that went down in between was reset (the VM restarted): what it reads now is
// what it counted since, which is what Prometheus's rate() takes it for too.
type Rates struct {
	Seconds float64 `json:"seconds"` // the interval between the two samples

	CPUPercent float64 `json:"cpu_percent"` // 100 is one CPU fully busy

	IOReadBytesPerSec  float64 `json:"io_read_bytes_per_sec"`
	IOWriteBytesPerSec float64 `json:"io_write_bytes_per_sec"`

	// Pressure is the share of the interval spent stalled, in percent.
	CPUSomePercent float64 `json:"cpu_some_percent"`
	MemSomePercent float64 `json:"memory_some_percent"`
	MemFullPercent float64 `json:"memory_full_percent"`
	IOSomePercent  float64 `json:"io_some_percent"`
	IOFullPercent  float64 `json:"io_full_percent"`

	NetRxBytesPerSec float64 `json:"net_rx_bytes_per_sec"`
	NetTxBytesPerSec float64 `json:"net_tx_bytes_per_sec"`
}

// Point is one interval of a sandbox's history: the rates over the interval
// that ended At, and its memory at that moment.
type Point struct {
	At          time.Time `json:"at"`
	CPUPercent  float32   `json:"cpu_percent"`
	MemoryBytes uint64    `json:"memory_bytes"`
	IOReadBPS   float32   `json:"io_read_bytes_per_sec"`
	IOWriteBPS  float32   `json:"io_write_bytes_per_sec"`
	CPUSome     float32   `json:"cpu_some_percent"`
	MemSome     float32   `json:"memory_some_percent"`
	MemFull     float32   `json:"memory_full_percent"`
	IOSome      float32   `json:"io_some_percent"`
	NetRxBPS    float32   `json:"net_rx_bytes_per_sec"`
	NetTxBPS    float32   `json:"net_tx_bytes_per_sec"`
}

// Sandbox is the latest sample of one sandbox's VM.
type Sandbox struct {
	ID         string    `json:"id"`                // the engine's id for it
	Owner      string    `json:"owner"`             // the owner's slug
	UserID     string    `json:"user_id,omitempty"` // the user that owns it
	Accounting string    `json:"accounting"`        // what Usage covers: cgroup, partial or process
	PID        int       `json:"pid,omitempty"`
	At         time.Time `json:"at"`
	Usage
	Net   *Net   `json:"net,omitempty"`   // nil without a gateway that reports it
	Rates *Rates `json:"rates,omitempty"` // nil on the first sample of a VM
}

// Owner is the latest sample of an owner's slice: the kernel's sum of its
// sandboxes' and its gateway's use.
type Owner struct {
	ID     string    `json:"id"` // the owner's slug
	UserID string    `json:"user_id,omitempty"`
	At     time.Time `json:"at"`
	Usage
	Rates *Rates `json:"rates,omitempty"`
}

// Netd is the latest sample of an owner's gateway process itself. What it
// carried for each guest is on that guest's Sandbox.Net.
type Netd struct {
	Owner      string    `json:"owner"`
	PID        int       `json:"pid"`
	Accounting string    `json:"accounting"`
	At         time.Time `json:"at"`
	Usage
	Rates    *Rates `json:"rates,omitempty"`
	StatsErr string `json:"stats_error,omitempty"` // why its per-guest figures are missing, if they are
}

// HostCPU is the host's CPU time in seconds, summed over all CPUs, cumulative.
type HostCPU struct {
	User    float64 `json:"user"`
	Nice    float64 `json:"nice"`
	System  float64 `json:"system"`
	Idle    float64 `json:"idle"`
	IOWait  float64 `json:"iowait"`
	IRQ     float64 `json:"irq"`
	SoftIRQ float64 `json:"softirq"`
	Steal   float64 `json:"steal"`
}

// HostMemory is the host's memory. Available is what a new workload could
// have without the host swapping: MemAvailable on Linux, the kernel's free
// percentage of memory on macOS.
type HostMemory struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	SwapTotalBytes uint64 `json:"swap_total_bytes"`
	SwapFreeBytes  uint64 `json:"swap_free_bytes"`
	// PressureLevel is macOS's own verdict: "normal", "warn" or "critical".
	// Empty where the host has pressure stall information instead.
	PressureLevel string `json:"pressure_level,omitempty"`
}

// Disk is the filesystem the daemon keeps its data on.
type Disk struct {
	Path       string `json:"path"`
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"` // what an unprivileged writer can still use
}

// Host is the latest sample of the machine bhatti runs on.
type Host struct {
	At       time.Time   `json:"at"`
	CPUs     int         `json:"cpus,omitempty"`
	CPU      *HostCPU    `json:"cpu,omitempty"`
	Memory   *HostMemory `json:"memory,omitempty"`
	Pressure *Pressure   `json:"pressure,omitempty"`
	Disk     *Disk       `json:"disk,omitempty"`
	// CPUPercent is the share of all CPUs that was busy over the last interval,
	// 0 to 100; nil on the first sample or where the host doesn't report CPU.
	CPUPercent *float64 `json:"cpu_percent,omitempty"`
}

// Snapshot is one complete sample. It is never modified once published, so a
// reader may keep it.
type Snapshot struct {
	At       time.Time     `json:"at"`
	Duration time.Duration `json:"duration_ns"` // how long taking it took
	Host     *Host         `json:"host,omitempty"`
	// Sandboxes, Owners and Netds are the ones that were measured this time,
	// ordered by id.
	Sandboxes []Sandbox `json:"sandboxes"`
	Owners    []Owner   `json:"owners"`
	Netds     []Netd    `json:"netds"`
}
