package collector

import (
	"io"
	"strings"

	"github.com/sahil-shubham/bhatti/pkg/promtext"
)

// RenderOptions shape the Prometheus text.
type RenderOptions struct {
	// SandboxLabel is the value of the sandbox label for the sandbox the engine
	// calls id: the label is what a person reads when working out who is using
	// the box, so a server that knows sandboxes by another name or id says it
	// here. nil uses the engine's id, which is the one in the sandbox's cgroup
	// scope and in the daemon's logs.
	SandboxLabel func(engineID string) string
}

func (o RenderOptions) sandbox(id string) string {
	if o.SandboxLabel == nil {
		return id
	}
	return o.SandboxLabel(id)
}

// WritePrometheus renders the latest snapshot as Prometheus text on w.
func (c *Collector) WritePrometheus(w io.Writer, opts RenderOptions) error {
	return c.Snapshot().WritePrometheus(w, opts)
}

// WritePrometheus renders the snapshot as Prometheus text on w.
func (s *Snapshot) WritePrometheus(w io.Writer, opts RenderOptions) error {
	pw := promtext.New(w)
	s.WriteTo(pw, opts)
	return pw.Flush()
}

// WriteTo adds the snapshot's families to pw, for a caller that has more to say
// in the same exposition and flushes it itself. Every family name starts with
// bhatti_sandbox_, bhatti_owner_, bhatti_netd_, bhatti_host_ or
// bhatti_collector_.
func (s *Snapshot) WriteTo(pw *promtext.Writer, opts RenderOptions) {
	if s.At.IsZero() {
		return // nothing has been sampled yet
	}
	pw.Family("bhatti_collector_last_sample_timestamp_seconds", "Unix time the figures below were read.", promtext.Gauge)
	pw.Sample("bhatti_collector_last_sample_timestamp_seconds", float64(s.At.UnixNano())/1e9)
	pw.Family("bhatti_collector_sample_duration_seconds", "How long reading them took.", promtext.Gauge)
	pw.Sample("bhatti_collector_sample_duration_seconds", s.Duration.Seconds())

	if s.Host != nil {
		writeHost(pw, s.Host)
	}

	owners := make([]usageRow, len(s.Owners))
	for i := range s.Owners {
		owners[i] = usageRow{labels: []string{"owner", s.Owners[i].ID}, usage: &s.Owners[i].Usage}
	}
	writeUsage(pw, "bhatti_owner_", "the owner's slice, which sums its sandboxes and its gateway", owners)

	if len(s.Sandboxes) > 0 {
		pw.Family("bhatti_sandbox_info", "A sandbox whose VM is running, and what its figures cover: cgroup, partial (placed after it started), process or none.", promtext.Gauge)
	}
	sandboxes := make([]usageRow, len(s.Sandboxes))
	for i := range s.Sandboxes {
		sb := &s.Sandboxes[i]
		labels := []string{"sandbox", opts.sandbox(sb.ID), "owner", sb.Owner}
		sandboxes[i] = usageRow{labels: labels, usage: &sb.Usage}
		pw.Sample("bhatti_sandbox_info", 1, "sandbox", labels[1], "owner", sb.Owner, "accounting", sb.Accounting)
	}
	writeUsage(pw, "bhatti_sandbox_", "the sandbox's VM", sandboxes)

	netds := make([]usageRow, len(s.Netds))
	for i := range s.Netds {
		netds[i] = usageRow{labels: []string{"owner", s.Netds[i].Owner}, usage: &s.Netds[i].Usage}
	}
	writeUsage(pw, "bhatti_netd_", "the owner's gateway", netds)
	writeGuestNet(pw, s, opts)
}

// usageRow is one series' worth of a usage: its labels and what was read.
type usageRow struct {
	labels []string
	usage  *Usage
}

// usageMetric is one family a usage yields. help's {} is the noun for what was
// measured, so the same family reads right for a sandbox, an owner or a gateway.
type usageMetric struct {
	name string // after the prefix
	help string
	typ  promtext.Type
	get  func(*Usage) (float64, bool)
}

func seconds(usec uint64) float64 { return float64(usec) / usecPerSec }

var usageMetrics = append([]usageMetric{
	{"cpu_usage_seconds_total", "CPU time used by {}, in seconds.", promtext.Counter, func(u *Usage) (float64, bool) {
		return cpuField(u, func(c *CPU) uint64 { return c.UsageUsec })
	}},
	{"cpu_user_seconds_total", "CPU time {} spent in user mode, in seconds.", promtext.Counter, func(u *Usage) (float64, bool) {
		return cpuField(u, func(c *CPU) uint64 { return c.UserUsec })
	}},
	{"cpu_system_seconds_total", "CPU time {} spent in the kernel, in seconds.", promtext.Counter, func(u *Usage) (float64, bool) {
		return cpuField(u, func(c *CPU) uint64 { return c.SystemUsec })
	}},
	{"memory_current_bytes", "Memory {} holds now, in bytes.", promtext.Gauge, func(u *Usage) (float64, bool) {
		if u.Memory == nil {
			return 0, false
		}
		return float64(u.Memory.Current), true
	}},
	{"memory_peak_bytes", "The most memory {} has held, in bytes.", promtext.Gauge, func(u *Usage) (float64, bool) {
		if u.Memory == nil || !u.Memory.HasPeak {
			return 0, false
		}
		return float64(u.Memory.Peak), true
	}},
	{"memory_anon_bytes", "Anonymous memory {} holds, in bytes.", promtext.Gauge, func(u *Usage) (float64, bool) {
		if u.Memory == nil || !u.Memory.HasStat {
			return 0, false
		}
		return float64(u.Memory.Anon), true
	}},
	{"memory_file_bytes", "File-backed memory {} holds, in bytes.", promtext.Gauge, func(u *Usage) (float64, bool) {
		if u.Memory == nil || !u.Memory.HasStat {
			return 0, false
		}
		return float64(u.Memory.File), true
	}},
	{"memory_oom_events_total", "Times the kernel found {} out of memory.", promtext.Counter, func(u *Usage) (float64, bool) {
		if u.Events == nil {
			return 0, false
		}
		return float64(u.Events.OOM), true
	}},
	{"memory_oom_kills_total", "Processes of {} the kernel killed for want of memory.", promtext.Counter, func(u *Usage) (float64, bool) {
		if u.Events == nil {
			return 0, false
		}
		return float64(u.Events.OOMKill), true
	}},
	{"io_read_bytes_total", "Bytes {} has read from block devices.", promtext.Counter, func(u *Usage) (float64, bool) {
		return ioField(u, false, func(i *IO) uint64 { return i.ReadBytes })
	}},
	{"io_write_bytes_total", "Bytes {} has written to block devices.", promtext.Counter, func(u *Usage) (float64, bool) {
		return ioField(u, false, func(i *IO) uint64 { return i.WriteBytes })
	}},
	{"io_read_ops_total", "Read requests {} has made to block devices.", promtext.Counter, func(u *Usage) (float64, bool) {
		return ioField(u, true, func(i *IO) uint64 { return i.ReadOps })
	}},
	{"io_write_ops_total", "Write requests {} has made to block devices.", promtext.Counter, func(u *Usage) (float64, bool) {
		return ioField(u, true, func(i *IO) uint64 { return i.WriteOps })
	}},
}, pressureAsUsage()...)

func cpuField(u *Usage, f func(*CPU) uint64) (float64, bool) {
	if u.CPU == nil {
		return 0, false
	}
	return seconds(f(u.CPU)), true
}

func ioField(u *Usage, ops bool, f func(*IO) uint64) (float64, bool) {
	if u.IO == nil || (ops && !u.IO.HasOps) {
		return 0, false
	}
	return float64(f(u.IO)), true
}

// pressureMetric is one family of pressure stall time. "waiting" is the time at
// least one task was stalled and "stalled" the time all of them were, the names
// node_exporter and cAdvisor give them.
type pressureMetric struct {
	name string
	help string
	get  func(*Pressure) (float64, bool)
}

var pressureMetrics = []pressureMetric{
	{"pressure_cpu_waiting_seconds_total", "Seconds at least one task of {} waited for a CPU.", func(p *Pressure) (float64, bool) {
		return psiField(p.CPU, false)
	}},
	{"pressure_cpu_stalled_seconds_total", "Seconds every runnable task of {} waited for a CPU.", func(p *Pressure) (float64, bool) {
		return psiField(p.CPU, true)
	}},
	{"pressure_memory_waiting_seconds_total", "Seconds at least one task of {} was stalled on memory.", func(p *Pressure) (float64, bool) {
		return psiField(p.Memory, false)
	}},
	{"pressure_memory_stalled_seconds_total", "Seconds every non-idle task of {} was stalled on memory.", func(p *Pressure) (float64, bool) {
		return psiField(p.Memory, true)
	}},
	{"pressure_io_waiting_seconds_total", "Seconds at least one task of {} was stalled on I/O.", func(p *Pressure) (float64, bool) {
		return psiField(p.IO, false)
	}},
	{"pressure_io_stalled_seconds_total", "Seconds every non-idle task of {} was stalled on I/O.", func(p *Pressure) (float64, bool) {
		return psiField(p.IO, true)
	}},
}

func psiField(p *PSI, full bool) (float64, bool) {
	switch {
	case p == nil:
		return 0, false
	case !full:
		return seconds(p.SomeUsec), true
	case p.HasFull:
		return seconds(p.FullUsec), true
	}
	return 0, false
}

// pressureAsUsage makes the pressure families readable from a Usage, for the
// families a sandbox, an owner and a gateway share.
func pressureAsUsage() []usageMetric {
	out := make([]usageMetric, len(pressureMetrics))
	for i, pm := range pressureMetrics {
		out[i] = usageMetric{name: pm.name, help: pm.help, typ: promtext.Counter, get: func(u *Usage) (float64, bool) {
			if u.Pressure == nil {
				return 0, false
			}
			return pm.get(u.Pressure)
		}}
	}
	return out
}

// writeUsage writes every family of usage metrics, one series per row that has
// the figure. A family nobody has a figure for is left out.
func writeUsage(pw *promtext.Writer, prefix, what string, rows []usageRow) {
	for _, m := range usageMetrics {
		name := prefix + m.name
		started := false
		for _, r := range rows {
			v, ok := m.get(r.usage)
			if !ok {
				continue
			}
			if !started {
				pw.Family(name, strings.Replace(m.help, "{}", what, 1), m.typ)
				started = true
			}
			pw.Sample(name, v, r.labels...)
		}
	}
}

// writeGuestNet writes what each owner's gateway carried for each guest.
func writeGuestNet(pw *promtext.Writer, s *Snapshot, opts RenderOptions) {
	type guest struct {
		sandbox, owner string
		net            *Net
	}
	var guests []guest
	for i := range s.Sandboxes {
		if sb := &s.Sandboxes[i]; sb.Net != nil {
			guests = append(guests, guest{opts.sandbox(sb.ID), sb.Owner, sb.Net})
		}
	}
	if len(guests) == 0 {
		return
	}
	counters := []struct {
		name, help string
		get        func(*Net) uint64
	}{
		{"bhatti_netd_rx_bytes_total", "Bytes the sandbox received through its gateway, as Ethernet frames.", func(n *Net) uint64 { return n.RxBytes }},
		{"bhatti_netd_rx_packets_total", "Frames the sandbox received through its gateway.", func(n *Net) uint64 { return n.RxPackets }},
		{"bhatti_netd_tx_bytes_total", "Bytes the sandbox sent through its gateway, as Ethernet frames, not counting what the gateway dropped.", func(n *Net) uint64 { return n.TxBytes }},
		{"bhatti_netd_tx_packets_total", "Frames the sandbox sent through its gateway, not counting what the gateway dropped.", func(n *Net) uint64 { return n.TxPackets }},
	}
	for _, c := range counters {
		pw.Family(c.name, c.help, promtext.Counter)
		for _, g := range guests {
			pw.Sample(c.name, float64(c.get(g.net)), "owner", g.owner, "sandbox", g.sandbox)
		}
	}
	const dropped = "bhatti_netd_dropped_frames_total"
	pw.Family(dropped, "What the gateway refused to carry for the sandbox: policy (its egress rules said no), spoof (the frame wasn't from the guest's own address) and hard_deny (a destination no rule can allow: the host, loopback, link-local and metadata addresses, other owners' networks).", promtext.Counter)
	for _, g := range guests {
		pw.Sample(dropped, float64(g.net.DroppedPolicy), "owner", g.owner, "sandbox", g.sandbox, "reason", "policy")
		pw.Sample(dropped, float64(g.net.DroppedSpoof), "owner", g.owner, "sandbox", g.sandbox, "reason", "spoof")
		pw.Sample(dropped, float64(g.net.DroppedHardDeny), "owner", g.owner, "sandbox", g.sandbox, "reason", "hard_deny")
	}
}

// writeHost writes the host's families.
func writeHost(pw *promtext.Writer, h *Host) {
	if h.CPUs > 0 {
		pw.Family("bhatti_host_cpus", "Logical CPUs the host has.", promtext.Gauge)
		pw.Sample("bhatti_host_cpus", float64(h.CPUs))
	}
	if c := h.CPU; c != nil {
		pw.Family("bhatti_host_cpu_seconds_total", "CPU time the host has spent in each mode, in seconds, summed over its CPUs.", promtext.Counter)
		for _, m := range []struct {
			mode string
			v    float64
		}{{"user", c.User}, {"nice", c.Nice}, {"system", c.System}, {"idle", c.Idle}, {"iowait", c.IOWait}, {"irq", c.IRQ}, {"softirq", c.SoftIRQ}, {"steal", c.Steal}} {
			pw.Sample("bhatti_host_cpu_seconds_total", m.v, "mode", m.mode)
		}
	}
	if m := h.Memory; m != nil {
		for _, g := range []struct {
			name, help string
			v          uint64
		}{
			{"bhatti_host_memory_total_bytes", "Memory the host has, in bytes.", m.TotalBytes},
			{"bhatti_host_memory_available_bytes", "Memory a new workload could have without the host swapping, in bytes: MemAvailable on Linux, the kernel's free share of memory on macOS.", m.AvailableBytes},
			{"bhatti_host_swap_total_bytes", "Swap the host has, in bytes.", m.SwapTotalBytes},
			{"bhatti_host_swap_free_bytes", "Swap the host has not used, in bytes.", m.SwapFreeBytes},
		} {
			pw.Family(g.name, g.help, promtext.Gauge)
			pw.Sample(g.name, float64(g.v))
		}
		if level, ok := map[string]float64{"normal": 0, "warn": 1, "critical": 2}[m.PressureLevel]; ok {
			pw.Family("bhatti_host_memory_pressure_level", "macOS's memory pressure verdict: 0 normal, 1 warn, 2 critical.", promtext.Gauge)
			pw.Sample("bhatti_host_memory_pressure_level", level)
		}
	}
	if h.Pressure != nil {
		for _, pm := range pressureMetrics {
			if pm.name == "pressure_cpu_stalled_seconds_total" {
				continue // the kernel keeps the host's cpu "full" line at zero, for compatibility
			}
			if v, ok := pm.get(h.Pressure); ok {
				name := "bhatti_host_" + pm.name
				pw.Family(name, strings.Replace(pm.help, "{}", "the host", 1), promtext.Counter)
				pw.Sample(name, v)
			}
		}
	}
	if d := h.Disk; d != nil {
		pw.Family("bhatti_host_disk_total_bytes", "Size of the filesystem the daemon keeps its data on, in bytes.", promtext.Gauge)
		pw.Sample("bhatti_host_disk_total_bytes", float64(d.TotalBytes), "path", d.Path)
		pw.Family("bhatti_host_disk_free_bytes", "Space left on that filesystem for an unprivileged writer, in bytes.", promtext.Gauge)
		pw.Sample("bhatti_host_disk_free_bytes", float64(d.FreeBytes), "path", d.Path)
	}
}
