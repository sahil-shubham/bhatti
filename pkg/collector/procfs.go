package collector

import (
	"bytes"
	"errors"
	"fmt"
)

// The host's /proc files, parsed from their bytes like the cgroup files are.

// userHZ is the unit of /proc/stat: ticks of 10 ms. The kernel's USER_HZ is 100
// on every architecture bhatti runs on, which is why tools hardcode it too.
const userHZ = 100

// parseProcStat reads the host's CPU time from /proc/stat: the aggregate "cpu"
// line, in seconds summed over all CPUs, and the number of "cpuN" lines.
//
//	cpu  3357 0 4313 1362393 1234 0 17 0 0 0
//	cpu0 1678 0 2156 681196 ...
func parseProcStat(b []byte) (cpu HostCPU, cpus int, err error) {
	var seen bool
	for len(b) > 0 {
		var line []byte
		line, b = nextLine(b)
		name, rest, _ := bytes.Cut(line, []byte(" "))
		if !bytes.HasPrefix(name, []byte("cpu")) {
			continue
		}
		if len(name) > 3 {
			cpus++ // cpu0, cpu1, ...
			continue
		}
		var f [8]uint64
		fields := bytes.Fields(rest)
		if len(fields) < 8 {
			return HostCPU{}, 0, fmt.Errorf("cpu line has %d fields, want at least 8", len(fields))
		}
		for i := range f {
			v, ok := atou(fields[i])
			if !ok {
				return HostCPU{}, 0, fmt.Errorf("cpu line: %q is not a number", fields[i])
			}
			f[i] = v
		}
		// user, nice, system, idle, iowait, irq, softirq, steal. The guest and
		// guest_nice columns that follow are already inside user and nice.
		sec := func(v uint64) float64 { return float64(v) / userHZ }
		cpu = HostCPU{User: sec(f[0]), Nice: sec(f[1]), System: sec(f[2]), Idle: sec(f[3]),
			IOWait: sec(f[4]), IRQ: sec(f[5]), SoftIRQ: sec(f[6]), Steal: sec(f[7])}
		seen = true
	}
	if !seen {
		return HostCPU{}, 0, errors.New("no cpu line")
	}
	return cpu, cpus, nil
}

// parseMeminfo reads /proc/meminfo's totals, which it gives in kibibytes.
func parseMeminfo(b []byte) (HostMemory, error) {
	var m HostMemory
	var total, avail, swapTotal, swapFree bool
	for len(b) > 0 {
		var line []byte
		line, b = nextLine(b)
		key, rest, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			continue
		}
		var into *uint64
		var seen *bool
		switch string(key) {
		case "MemTotal":
			into, seen = &m.TotalBytes, &total
		case "MemAvailable":
			into, seen = &m.AvailableBytes, &avail
		case "SwapTotal":
			into, seen = &m.SwapTotalBytes, &swapTotal
		case "SwapFree":
			into, seen = &m.SwapFreeBytes, &swapFree
		default:
			continue
		}
		fields := bytes.Fields(rest)
		if len(fields) != 2 || string(fields[1]) != "kB" {
			return HostMemory{}, fmt.Errorf("line %q: want a number and kB", line)
		}
		kib, ok := atou(fields[0])
		if !ok || kib > (1<<64-1)/1024 {
			return HostMemory{}, fmt.Errorf("line %q: %q is not a number", line, fields[0])
		}
		*into, *seen = kib*1024, true
	}
	if !total || !avail || !swapTotal || !swapFree {
		return HostMemory{}, errors.New("meminfo lacks MemTotal, MemAvailable, SwapTotal or SwapFree")
	}
	return m, nil
}

// readProc reads /proc/stat, /proc/meminfo and /proc/pressure/*, and the disk.
// A part that can't be read is left out, and the error says which; pressure
// the kernel doesn't have is no error.
func (h *hostReader) readProc() (Host, error) {
	var host Host
	var errs []error

	if b, err := h.rd.read(h.procRoot + "/stat"); err != nil {
		errs = append(errs, fmt.Errorf("/proc/stat: %w", err))
	} else if cpu, cpus, err := parseProcStat(b); err != nil {
		errs = append(errs, fmt.Errorf("/proc/stat: %w", err))
	} else {
		host.CPU, host.CPUs = &cpu, cpus
	}

	if b, err := h.rd.read(h.procRoot + "/meminfo"); err != nil {
		errs = append(errs, fmt.Errorf("/proc/meminfo: %w", err))
	} else if mem, err := parseMeminfo(b); err != nil {
		errs = append(errs, fmt.Errorf("/proc/meminfo: %w", err))
	} else {
		host.Memory = &mem
	}

	var press Pressure
	for _, p := range []struct {
		name string
		into **PSI
	}{{"cpu", &press.CPU}, {"memory", &press.Memory}, {"io", &press.IO}} {
		b, err := h.rd.read(h.procRoot + "/pressure/" + p.name)
		if err != nil {
			if !absent(err) {
				errs = append(errs, fmt.Errorf("/proc/pressure/%s: %w", p.name, err))
			}
			continue
		}
		if psi, err := parsePSI(b); err != nil {
			errs = append(errs, fmt.Errorf("/proc/pressure/%s: %w", p.name, err))
		} else {
			*p.into = &psi
		}
	}
	if press != (Pressure{}) {
		host.Pressure = &press
	}

	if h.dataDir != "" {
		if d, err := readDisk(h.dataDir); err != nil {
			errs = append(errs, err)
		} else {
			host.Disk = d
		}
	}
	return host, errors.Join(errs...)
}
