package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The MB fields are MiB: /proc reports memory in KiB, which is divided by 1024.
// Scope applies to the unprefixed VMM fields only. HostVMM, daemon, netd, CPU,
// memory, and load fields always describe the entire host. PSS sums and slices
// are partial when PSSComplete is false (for example, without permission to
// read a confined VMM's smaps_rollup); missing PSS is never counted as zero.
type HostSample struct {
	At                      time.Time `json:"at"`
	Supported               bool      `json:"supported"`
	Reason                  string    `json:"reason,omitempty"`
	Scope                   string    `json:"scope"`
	ScopeReason             string    `json:"scope_reason,omitempty"`
	CPUDeltaAvailable       bool      `json:"cpu_delta_available"`
	CPUPercent              float64   `json:"cpu_percent"`
	MemAvailableMB          float64   `json:"mem_available_mb"`
	Load1                   float64   `json:"load1"`
	ProcessCPUDeltaComplete bool      `json:"process_cpu_delta_complete"`
	PSSComplete             bool      `json:"pss_complete"`
	PSSUnavailableCount     int       `json:"pss_unavailable_count"`
	DaemonCount             int       `json:"daemon_count"`
	DaemonCPUPercent        float64   `json:"daemon_cpu_percent"`
	DaemonPSSMB             float64   `json:"daemon_pss_mb"`
	NetdCount               int       `json:"netd_count"`
	NetdCPUPercent          float64   `json:"netd_cpu_percent"`
	NetdPSSMB               float64   `json:"netd_pss_mb"`
	VMMCount                int       `json:"vmm_count"`
	VMMCPUPercent           float64   `json:"vmm_cpu_percent"`
	VMMPSSMB                float64   `json:"vmm_pss_mb"`
	VMMPSSValuesMB          []float64 `json:"vmm_pss_values_mb"`
	HostVMMCount            int       `json:"host_vmm_count"`
	HostVMMCPUPercent       float64   `json:"host_vmm_cpu_percent"`
	HostVMMPSSMB            float64   `json:"host_vmm_pss_mb"`
	HostVMMPSSValuesMB      []float64 `json:"host_vmm_pss_values_mb"`
}

type processTicks struct {
	start, used uint64
	seen        uint64
	owner       string // immutable user_id from state.json, cached only for this PID/start time
	attributed  bool
}

type HostSampler struct {
	writer     io.Writer
	dataDir    string
	benchOwner string

	mu        sync.Mutex // Sample may be called while Run is sampling
	sequence  uint64
	prevTotal uint64
	prevIdle  uint64
	processes map[int]processTicks
}

func newHostSampler(w io.Writer, dataDir, benchOwner string) *HostSampler {
	absoluteDir := ""
	if dataDir != "" {
		if resolved, err := filepath.Abs(dataDir); err == nil {
			absoluteDir = resolved
		}
	}
	return &HostSampler{
		writer: w, dataDir: absoluteDir, benchOwner: benchOwner,
		processes: make(map[int]processTicks),
	}
}

// SetBenchOwner selects the exact daemon user ID after it becomes known from
// the API; it is safe to call while Run is sampling.
func (s *HostSampler) SetBenchOwner(owner string) {
	s.mu.Lock()
	s.benchOwner = owner
	s.mu.Unlock()
}

func (s *HostSampler) Sample(now time.Time) HostSample {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := HostSample{At: now, Scope: "host_total", PSSComplete: true}
	if runtime.GOOS != "linux" {
		result.PSSComplete = false
		result.Reason = "host process sampling requires Linux /proc"
		result.ScopeReason = "process attribution requires Linux /proc; host totals unavailable"
		return result
	}

	cpu, err := os.ReadFile("/proc/stat")
	if err != nil {
		return s.unsupported(result, fmt.Sprintf("/proc/stat inaccessible: %v", err))
	}
	total, idle, cores, err := cpuCounters(cpu)
	if err != nil {
		return s.unsupported(result, fmt.Sprintf("/proc/stat invalid: %v", err))
	}
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return s.unsupported(result, fmt.Sprintf("/proc/meminfo inaccessible: %v", err))
	}
	available, ok := meminfoKiB(meminfo, "MemAvailable:")
	if !ok {
		return s.unsupported(result, "/proc/meminfo has no MemAvailable")
	}
	loadavg, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return s.unsupported(result, fmt.Sprintf("/proc/loadavg inaccessible: %v", err))
	}
	load, err := parseLoad1(loadavg)
	if err != nil {
		return s.unsupported(result, fmt.Sprintf("/proc/loadavg invalid: %v", err))
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return s.unsupported(result, fmt.Sprintf("/proc processes inaccessible: %v", err))
	}

	result.Supported = true
	result.MemAvailableMB = float64(available) / 1024
	result.Load1 = load
	result.CPUDeltaAvailable = s.prevTotal != 0 && total > s.prevTotal && idle >= s.prevIdle && idle-s.prevIdle <= total-s.prevTotal
	if result.CPUDeltaAvailable {
		result.CPUPercent = 100 * float64(total-s.prevTotal-(idle-s.prevIdle)) / float64(total-s.prevTotal)
	}
	result.ProcessCPUDeltaComplete = result.CPUDeltaAvailable && cores > 0
	var totalDelta uint64
	if result.CPUDeltaAvailable {
		totalDelta = total - s.prevTotal
	}
	s.prevTotal, s.prevIdle = total, idle
	s.sequence++

	var benchCount int
	var benchCPU, benchPSS float64
	var benchPSSValues []float64
	var unattributed int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		procDir := filepath.Join("/proc", entry.Name())
		kind, specPath := identifyHostProcess(procDir)
		if kind == "" {
			continue
		}
		previous := s.processes[pid]
		stat, statErr := os.ReadFile(filepath.Join(procDir, "stat"))
		used, start, statOK := procStatTicks(stat)
		if statErr != nil || !statOK {
			result.ProcessCPUDeltaComplete = false
			delete(s.processes, pid)
		}
		var processCPU float64
		if statErr == nil && statOK {
			if previous.start == start && previous.seen != 0 && used >= previous.used && result.CPUDeltaAvailable && cores > 0 {
				// /proc/stat is the sum of all online CPUs; convert the process's
				// share of those ticks back to one-core CPU percentage.
				processCPU = 100 * float64(used-previous.used) * float64(cores) / float64(totalDelta)
			} else {
				result.ProcessCPUDeltaComplete = false
				previous.owner, previous.attributed = "", false
			}
			previous.start, previous.used, previous.seen = start, used, s.sequence
			s.processes[pid] = previous
		}
		pssKiB, pssErr := procPSSKiB(procDir)
		if pssErr != nil {
			result.PSSComplete = false
			result.PSSUnavailableCount++
		}
		pssMB := float64(pssKiB) / 1024
		switch kind {
		case "daemon":
			result.DaemonCount++
			result.DaemonCPUPercent += processCPU
			if pssErr == nil {
				result.DaemonPSSMB += pssMB
			}
		case "netd":
			result.NetdCount++
			result.NetdCPUPercent += processCPU
			if pssErr == nil {
				result.NetdPSSMB += pssMB
			}
		case "vmm":
			result.HostVMMCount++
			result.HostVMMCPUPercent += processCPU
			if pssErr == nil {
				result.HostVMMPSSMB += pssMB
				result.HostVMMPSSValuesMB = append(result.HostVMMPSSValuesMB, pssMB)
			}
			if s.benchOwner == "" {
				continue
			}
			owner, known := previous.owner, previous.attributed && statErr == nil && statOK && previous.start == start
			if !known {
				owner, known = s.vmOwner(pid, specPath)
				if statErr == nil && statOK && known {
					previous.owner, previous.attributed = owner, true
					s.processes[pid] = previous
				}
			}
			if !known {
				unattributed++
				continue
			}
			if owner == s.benchOwner {
				benchCount++
				benchCPU += processCPU
				if pssErr == nil {
					benchPSS += pssMB
					benchPSSValues = append(benchPSSValues, pssMB)
				}
			}
		}
	}
	for pid, previous := range s.processes {
		if previous.seen != s.sequence {
			delete(s.processes, pid)
		}
	}

	result.VMMCount, result.VMMCPUPercent, result.VMMPSSMB, result.VMMPSSValuesMB =
		result.HostVMMCount, result.HostVMMCPUPercent, result.HostVMMPSSMB, result.HostVMMPSSValuesMB
	switch {
	case s.benchOwner == "":
		result.ScopeReason = "benchmark owner ID unavailable; VMM totals include other users' VMs"
	case unattributed != 0:
		result.ScopeReason = fmt.Sprintf("%d active VMM(s) could not be linked to a user_id; VMM totals include other users' VMs", unattributed)
	default:
		result.Scope = "bench_only"
		result.ScopeReason = "every observed VMM matched its spec path and state.json user_id; daemon and netd metrics remain host-wide"
		result.VMMCount, result.VMMCPUPercent, result.VMMPSSMB, result.VMMPSSValuesMB =
			benchCount, benchCPU, benchPSS, benchPSSValues
	}
	if !result.PSSComplete {
		result.Reason = fmt.Sprintf("smaps_rollup unavailable for %d observed process(es); PSS totals are partial", result.PSSUnavailableCount)
	}
	return result
}

func (s *HostSampler) unsupported(result HostSample, reason string) HostSample {
	s.prevTotal, s.prevIdle = 0, 0
	clear(s.processes)
	result.PSSComplete = false
	result.Reason = reason
	result.ScopeReason = "host totals unavailable; process attribution requires readable /proc"
	return result
}

func (s *HostSampler) Run(ctx context.Context) {
	if s.writer == nil {
		log.Print("host sampler: no JSONL writer")
		return
	}
	encoder := json.NewEncoder(s.writer)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := encoder.Encode(s.Sample(time.Now())); err != nil {
			log.Printf("host sampler: write JSONL: %v", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// /proc/stat includes guest ticks in user/nice already; exclude its guest
// columns to avoid counting that CPU time twice. Idle includes iowait.
func cpuCounters(data []byte) (total, idle uint64, cores int, err error) {
	line, remaining, _ := bytes.Cut(data, []byte{'\n'})
	fields := bytes.Fields(line)
	if len(fields) < 5 || !bytes.Equal(fields[0], []byte("cpu")) {
		return 0, 0, 0, fmt.Errorf("missing aggregate cpu line")
	}
	for i := 1; i < len(fields) && i <= 8; i++ {
		v, parseErr := strconv.ParseUint(string(fields[i]), 10, 64)
		if parseErr != nil {
			return 0, 0, 0, parseErr
		}
		total += v
		if i == 4 || i == 5 {
			idle += v
		}
	}
	for len(remaining) != 0 {
		line, remaining, _ = bytes.Cut(remaining, []byte{'\n'})
		if len(line) > 3 && bytes.HasPrefix(line, []byte("cpu")) && line[3] >= '0' && line[3] <= '9' {
			cores++
		}
	}
	return total, idle, cores, nil
}

func meminfoKiB(data []byte, key string) (uint64, bool) {
	for len(data) != 0 {
		line, remaining, _ := bytes.Cut(data, []byte{'\n'})
		data = remaining
		if bytes.HasPrefix(line, []byte(key)) {
			fields := bytes.Fields(line[len(key):])
			if len(fields) > 0 {
				v, err := strconv.ParseUint(string(fields[0]), 10, 64)
				return v, err == nil
			}
			return 0, false
		}
	}
	return 0, false
}

func parseLoad1(data []byte) (float64, error) {
	fields := bytes.Fields(data)
	if len(fields) == 0 {
		return 0, fmt.Errorf("missing load average")
	}
	return strconv.ParseFloat(string(fields[0]), 64)
}

// Check comm before cmdline to avoid opening command lines for every process.
// The command line is authoritative: similarly named unrelated processes must
// not be counted, and a capabilities probe is not a running VMM.
func identifyHostProcess(procDir string) (kind, specPath string) {
	comm, err := os.ReadFile(filepath.Join(procDir, "comm"))
	if err != nil {
		return "", ""
	}
	name := string(bytes.TrimSpace(comm))
	if name != "bhatti" && name != "bhatti-netd" && name != "bhatti-vmm" {
		return "", ""
	}
	cmdline, err := os.ReadFile(filepath.Join(procDir, "cmdline"))
	if err != nil {
		return "", ""
	}
	argv0, args, found := bytes.Cut(cmdline, []byte{0})
	if !found || filepath.Base(string(argv0)) != name {
		return "", ""
	}
	switch name {
	case "bhatti":
		for len(args) > 0 {
			arg, rest, _ := bytes.Cut(args, []byte{0})
			if bytes.Equal(arg, []byte("serve")) {
				return "daemon", ""
			}
			args = rest
		}
	case "bhatti-netd":
		return "netd", ""
	case "bhatti-vmm":
		path, rest, found := bytes.Cut(args, []byte{0})
		if found && len(rest) == 0 && filepath.Base(string(path)) == "vmspec.json" {
			return "vmm", string(path)
		}
	}
	return "", ""
}

// /proc/PID/stat's comm is parenthesized and may contain spaces or ')' itself.
// Fields 14/15 are utime/stime; 22 is the process start tick, used to reject
// PID reuse when computing CPU deltas and caching ownership.
func procStatTicks(data []byte) (used, start uint64, ok bool) {
	closing := bytes.LastIndexByte(data, ')')
	if closing < 0 || closing+2 >= len(data) || data[closing+1] != ' ' {
		return 0, 0, false
	}
	fields := data[closing+2:]
	var user, system uint64
	for field := range 20 {
		for len(fields) > 0 && fields[0] <= ' ' {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			return 0, 0, false
		}
		end := 0
		for end < len(fields) && fields[end] > ' ' {
			end++
		}
		var err error
		switch field {
		case 11:
			user, err = strconv.ParseUint(string(fields[:end]), 10, 64)
		case 12:
			system, err = strconv.ParseUint(string(fields[:end]), 10, 64)
		case 19:
			start, err = strconv.ParseUint(string(fields[:end]), 10, 64)
		}
		if err != nil {
			return 0, 0, false
		}
		fields = fields[end:]
	}
	return user + system, start, start != 0
}

func procPSSKiB(procDir string) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(procDir, "smaps_rollup"))
	if err != nil {
		return 0, err
	}
	for len(data) != 0 {
		line, remaining, _ := bytes.Cut(data, []byte{'\n'})
		data = remaining
		if bytes.HasPrefix(line, []byte("Pss:")) {
			fields := bytes.Fields(line[len("Pss:"):])
			if len(fields) > 0 {
				return strconv.ParseUint(string(fields[0]), 10, 64)
			}
			break
		}
	}
	return 0, fmt.Errorf("smaps_rollup missing Pss")
}

// The spec argument and the durable owner record together bind a live VMM to
// an exact user ID. A sandbox name, UID, or substring in argv cannot prove it.
func (s *HostSampler) vmOwner(pid int, specPath string) (string, bool) {
	if s.dataDir == "" || !filepath.IsAbs(specPath) {
		return "", false
	}
	sandboxDir := filepath.Dir(specPath)
	id := filepath.Base(sandboxDir)
	if id == "." || id == "" || filepath.Join(s.dataDir, "sandboxes", id, "vmspec.json") != specPath {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(sandboxDir, "state.json"))
	if err != nil {
		return "", false
	}
	var record struct {
		ID        string `json:"id"`
		UserID    string `json:"user_id"`
		HelperPID int    `json:"helper_pid"`
	}
	if json.Unmarshal(data, &record) != nil || record.ID != id || record.UserID == "" || record.HelperPID != pid {
		return "", false
	}
	return record.UserID, true
}

// hostInfo records static machine identity independently of whether /proc can
// be sampled. Unavailable values are labeled rather than invented.
func hostInfo(dataDir string) map[string]any {
	info := map[string]any{
		"os": runtime.GOOS, "arch": runtime.GOARCH, "data_dir": dataDir,
		"cpu_model": "unavailable", "logical_cores": nil, "ram_mb": nil,
		"kernel": "unavailable", "filesystem": filesystemName(dataDir),
	}
	if hostname, err := os.Hostname(); err == nil {
		info["hostname"] = hostname
	} else {
		info["hostname"] = "unavailable: " + err.Error()
	}
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, key := range []string{"model name", "Hardware", "Processor"} {
				if value := cpuinfoValue(data, key); value != "" {
					info["cpu_model"] = value
					break
				}
			}
		}
		if data, err := os.ReadFile("/proc/stat"); err == nil {
			if _, _, cores, err := cpuCounters(data); err == nil && cores > 0 {
				info["logical_cores"] = cores
			}
		}
		if data, err := os.ReadFile("/proc/meminfo"); err == nil {
			if total, ok := meminfoKiB(data, "MemTotal:"); ok {
				info["ram_mb"] = float64(total) / 1024
			}
		}
		if release, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
			info["kernel"] = strings.TrimSpace(string(release))
		}
	} else if runtime.GOOS == "darwin" {
		info["logical_cores"] = runtime.NumCPU()
		if value := systemCommand("sysctl", "-n", "machdep.cpu.brand_string"); value != "" {
			info["cpu_model"] = value
		} else if value := systemCommand("sysctl", "-n", "hw.model"); value != "" {
			info["machine_model"] = value // hardware identifier, not a CPU model
		}
		if value := systemCommand("sysctl", "-n", "hw.memsize"); value != "" {
			if total, err := strconv.ParseUint(value, 10, 64); err == nil {
				info["ram_mb"] = float64(total) / (1024 * 1024)
			}
		}
		if value := systemCommand("uname", "-r"); value != "" {
			info["kernel"] = value
		}
	}
	return info
}

func cpuinfoValue(data []byte, key string) string {
	for len(data) > 0 {
		line, remaining, _ := bytes.Cut(data, []byte{'\n'})
		data = remaining
		label, value, found := bytes.Cut(line, []byte{':'})
		if found && string(bytes.TrimSpace(label)) == key {
			return string(bytes.TrimSpace(value))
		}
	}
	return ""
}

func systemCommand(name string, args ...string) string {
	data, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func filesystemName(path string) string {
	if path == "" {
		return "unavailable: data directory unspecified"
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return "unavailable: " + err.Error()
	}
	// The same syscall has a numeric Type on Linux and a Fstypename byte
	// array on macOS. Reflection keeps this file buildable on both platforms.
	value := reflect.ValueOf(stat)
	if name := value.FieldByName("Fstypename"); name.IsValid() {
		var fs strings.Builder
		for i := range name.Len() {
			var char byte
			if name.Index(i).Kind() == reflect.Int8 {
				char = byte(name.Index(i).Int())
			} else {
				char = byte(name.Index(i).Uint())
			}
			if char == 0 {
				break
			}
			fs.WriteByte(char)
		}
		return fs.String()
	}
	if kind := value.FieldByName("Type"); kind.IsValid() {
		fsType := uint64(kind.Int())
		switch fsType {
		case 0xef53:
			return "ext4"
		case 0x58465342:
			return "xfs"
		case 0x9123683e:
			return "btrfs"
		case 0x01021994:
			return "tmpfs"
		case 0x794c7630:
			return "overlayfs"
		case 0x65735546:
			return "fuse"
		case 0x6969:
			return "nfs"
		case 0x2fc12fc1:
			return "zfs"
		default:
			return fmt.Sprintf("type_0x%x", fsType)
		}
	}
	return "unavailable: unknown statfs layout"
}
