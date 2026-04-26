//go:build linux

package firecracker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// --- Create ---

func (e *Engine) Create(ctx context.Context, spec engine.SandboxSpec) (info engine.SandboxInfo, err error) {
	createStart := time.Now()
	phase := func(name string) {
		slog.Debug("create.phase", "sandbox", spec.Name, "phase", name,
			"elapsed_ms", time.Since(createStart).Milliseconds())
	}

	id := generateID()
	sandboxDir := filepath.Join(e.cfg.DataDir, "sandboxes", id)
	os.MkdirAll(sandboxDir, 0700)

	// Deferred cleanup: on any error, release all resources acquired so far.
	// Uses named return `err` so the defer sees the final error value.
	var (
		guestIP  string
		tapName  string
		vmCancel context.CancelFunc
		fcCmd    *exec.Cmd
	)
	defer func() {
		if err != nil {
			if fcCmd != nil && fcCmd.Process != nil {
				killFC(fcCmd, 1*time.Second)
			}
			if vmCancel != nil {
				vmCancel()
			}
			if tapName != "" {
				destroyTapDevice(tapName)
			}
			if guestIP != "" {
				e.mu.RLock()
				if net, ok := e.userNetworks[spec.UserID]; ok {
					net.Pool.Release(guestIP)
				}
				e.mu.RUnlock()
			}
			os.RemoveAll(sandboxDir)
			// Clean up jailer chroot — without this, failed creates
			// leak jail dirs under jails/firecracker/<id>/.
			if e.cfg.jailed() {
				os.RemoveAll(filepath.Join(e.cfg.DataDir, "jails", "firecracker", id))
			}
		}
	}()

	// 1. Copy rootfs (from resolved image path or default base)
	phase("rootfs_copy_start")
	baseImage := e.cfg.BaseRootfs
	if spec.BaseImage != "" {
		baseImage = spec.BaseImage
	}
	rootfsPath := filepath.Join(sandboxDir, "rootfs.ext4")
	if err = copyRootfs(baseImage, rootfsPath); err != nil {
		return info, fmt.Errorf("copy rootfs: %w", err)
	}
	phase("rootfs_copy_done")

	// 1b. Ensure rootfs has the current lohar to prevent protocol drift.
	// The install script injects lohar into base images and writes a stamp
	// file. If the stamp matches, the reflink copy already has the right
	// binary and we skip the expensive mount+cp+umount (~80ms). This is
	// the common path for stock images after a clean install/upgrade.
	// For non-stamped images (saved, imported, manual), inject as fallback.
	phase("lohar_inject_start")
	if loharNeedsInjection(baseImage, e.cfg.DataDir, e.loharHash) {
		if err = injectLoharIntoRootfs(rootfsPath, e.cfg.DataDir); err != nil {
			slog.Warn("lohar injection failed", "error", err)
			// Non-fatal — image's lohar may work, but warn loudly
		}
	}
	phase("lohar_inject_done")

	// 1c. Resize rootfs if requested
	if spec.DiskSizeMB > 0 {
		phase("resize_start")
		exec.Command("e2fsck", "-f", "-y", rootfsPath).Run() // best effort
		phase("e2fsck_done")
		if err = exec.Command("truncate", "-s", fmt.Sprintf("%dM", spec.DiskSizeMB), rootfsPath).Run(); err != nil {
			return info, fmt.Errorf("resize rootfs: %w", err)
		}
		if err = exec.Command("resize2fs", rootfsPath).Run(); err != nil {
			return info, fmt.Errorf("resize2fs: %w", err)
		}
		phase("resize_done")
	}

	// 2. Allocate CID and paths
	cid := atomic.AddUint32(&e.nextCID, 1)
	socketPath := filepath.Join(sandboxDir, "firecracker.sock")
	vsockPath := filepath.Join(sandboxDir, "vsock.sock")

	// 3. Get or create user's network, allocate IP, create TAP
	phase("network_start")
	userNet := e.getOrCreateUserNetwork(spec.UserID, spec.SubnetIndex)
	if err = ensureUserBridge(userNet); err != nil {
		return info, fmt.Errorf("setup user bridge: %w", err)
	}
	guestIP, err = userNet.Pool.Allocate()
	if err != nil {
		return info, fmt.Errorf("allocate IP: %w", err)
	}
	tapName, err = createTapDevice(id, userNet.BridgeName)
	if err != nil {
		return info, fmt.Errorf("create tap: %w", err)
	}

	// Flush stale ARP entry for this IP. When a sandbox is destroyed and
	// a new one reuses the same IP, the host ARP cache still maps the IP
	// to the old sandbox's MAC (STALE, gc_stale_time=60s). The new VM
	// gets a fresh MAC, so the host sends TCP SYNs to the old MAC —
	// which no longer exists on any TAP. WaitReady times out at 30s.
	exec.Command("ip", "neigh", "del", guestIP, "dev", userNet.BridgeName).Run() // best-effort
	phase("network_done")

	// 4. Compute VM config
	vcpuCount := int64(spec.CPUs)
	if vcpuCount < 1 {
		vcpuCount = 1
	}
	memMB := int64(spec.MemoryMB)
	if memMB < 128 {
		memMB = 2048
	}
	mac := generateMAC()

	// 4b. Generate auth token
	tokenBytes := make([]byte, 16)
	rand.Read(tokenBytes)
	token := fmt.Sprintf("%x", tokenBytes)

	// 4c. Build config drive
	envMap := make(map[string]string)
	for k, v := range spec.Env {
		envMap[k] = v
	}
	filesMap := make(map[string]ConfigFile)
	for path, f := range spec.Files {
		filesMap[path] = ConfigFile{
			Content: base64.StdEncoding.EncodeToString(f.Content),
			Mode:    f.Mode,
		}
	}

	configDrivePath := filepath.Join(sandboxDir, "config.ext4")
	var volumeMounts []VolumeMountConfig
	var volAttachments []VolumeAttachmentInfo
	driveIndex := byte('c') // vdb=config, vdc=first vol, vdd=second, ...

	// Maximum 24 volumes per sandbox (vdc through vdz)
	const maxVolumesPerSandbox = 24
	totalVols := len(spec.ResolvedVolumes) + len(spec.NewVolumes)
	if totalVols > maxVolumesPerSandbox {
		return info, fmt.Errorf("too many volumes: %d (max %d)", totalVols, maxVolumesPerSandbox)
	}

	// Persistent volumes (resolved by server layer)
	for _, vol := range spec.ResolvedVolumes {
		device := fmt.Sprintf("/dev/vd%c", driveIndex)
		volumeMounts = append(volumeMounts, VolumeMountConfig{
			Device: device, Mount: vol.Mount, FS: "ext4", ReadOnly: vol.ReadOnly,
		})
		volAttachments = append(volAttachments, VolumeAttachmentInfo{
			DriveID: vol.DriveID, Name: vol.Name, FilePath: vol.FilePath,
			Mount: vol.Mount, ReadOnly: vol.ReadOnly,
		})
		driveIndex++
	}

	// Legacy ephemeral volumes (created in sandbox dir, destroyed with sandbox)
	for _, vs := range spec.NewVolumes {
		volPath := filepath.Join(sandboxDir, fmt.Sprintf("vol-%s.ext4", vs.Name))
		if err = createVolume(volPath, vs.SizeMB); err != nil {
			return info, fmt.Errorf("create volume %s: %w", vs.Name, err)
		}
		device := fmt.Sprintf("/dev/vd%c", driveIndex)
		volumeMounts = append(volumeMounts, VolumeMountConfig{
			Device: device, Mount: vs.Mount, FS: "ext4",
		})
		driveIndex++
	}

	name := spec.Name
	if name == "" {
		name = id
	}

	phase("config_drive_start")
	if err = createConfigDrive(configDrivePath, SandboxConfig{
		SandboxID: id,
		Hostname:  name,
		Token:     token,
		Env:       envMap,
		Files:     filesMap,
		Volumes:   volumeMounts,
		Init:      spec.Init,
		DNS:       []string{"1.1.1.1", "8.8.8.8"},
		User:      "lohar",
	}); err != nil {
		return info, fmt.Errorf("create config drive: %w", err)
	}
	phase("config_drive_done")

	// 5. Build path resolver for FC API calls
	phase("resolve_paths")
	jp := newJailPaths(e.cfg.jailed())

	// Resolve ALL paths FC will reference — must happen before startFC
	// so the files map includes everything that needs hard-linking.
	kernelPath := jp.resolve("kernel", e.cfg.KernelPath)
	rootfsRef := jp.resolve("rootfs.ext4", rootfsPath)
	configRef := jp.resolve("config.ext4", configDrivePath)
	vsockRef := jp.chrootPath("vsock.sock", vsockPath)
	logRef := jp.chrootPath("firecracker.log", filepath.Join(sandboxDir, "firecracker.log"))
	metricsRef := jp.chrootPath("firecracker.metrics", filepath.Join(sandboxDir, "firecracker.metrics"))

	// Pre-resolve volume paths so they're in the files map for hard-linking
	volRefs := make(map[string]string) // driveID → resolved path
	for _, vol := range spec.ResolvedVolumes {
		volRefs[vol.DriveID] = jp.resolve(fmt.Sprintf("vol-%s.ext4", vol.Name), vol.FilePath)
	}
	ephVolRefs := make(map[string]string)
	for _, vs := range spec.NewVolumes {
		volPath := filepath.Join(sandboxDir, fmt.Sprintf("vol-%s.ext4", vs.Name))
		ephVolRefs[vs.Name] = jp.resolve(fmt.Sprintf("ephvol-%s.ext4", vs.Name), volPath)
	}

	// Start Firecracker process
	phase("fc_start_begin")
	fcProc, err := e.startFC(socketPath, startFCOpts{
		id: id, vcpus: vcpuCount, memMB: memMB, files: jp.files,
	})
	if err != nil {
		return info, err
	}
	phase("fc_start_done")
	fcCmd = fcProc.cmd
	vmCancel = fcProc.cancel
	stderrBuf := fcProc.stderrBuf

	// In jailed mode, the API socket is inside the chroot
	apiSocket := socketPath
	if fcProc.socket != "" {
		apiSocket = fcProc.socket
	}

	// 6. Configure via HTTP API
	phase("fc_api_start")
	client := fcAPIClient(apiSocket)

	// FC logger and metrics must be set before any other configuration.
	// Warning level only — Debug is guest-influenceable. Non-fatal if setup fails.
	if err = fcPut(ctx, client, "/logger", fmt.Sprintf(
		`{"log_path":%q,"level":"Warning","show_level":true,"show_log_origin":true}`,
		logRef)); err != nil {
		slog.Warn("FC logger setup failed", "error", err)
	}
	if err = fcPut(ctx, client, "/metrics", fmt.Sprintf(
		`{"metrics_path":%q}`, metricsRef)); err != nil {
		slog.Warn("FC metrics setup failed", "error", err)
	}

	// Boot args include ip= for kernel-level network configuration.
	// Uses the user's bridge gateway instead of a hardcoded IP.
	bootArgs := fmt.Sprintf(
		"reboot=k panic=1 pci=off 8250.nr_uarts=0 init=/usr/local/bin/lohar quiet loglevel=0 ip=%s::%s:255.255.255.0::eth0:off:1.1.1.1:8.8.8.8:",
		guestIP, userNet.GatewayIP)

	if err = fcPut(ctx, client, "/boot-source", fmt.Sprintf(
		`{"kernel_image_path":%q,"boot_args":%q}`,
		kernelPath, bootArgs)); err != nil {
		return info, fmt.Errorf("set boot-source: %w", err)
	}

	rootfsDrive := fmt.Sprintf(`{"drive_id":"rootfs","path_on_host":%q,"is_root_device":true,"is_read_only":false`, rootfsRef)
	if bw := e.cfg.RateLimits.diskBandwidth(); bw > 0 {
		iops := e.cfg.RateLimits.diskIOPS()
		rootfsDrive += fmt.Sprintf(`,"rate_limiter":{"bandwidth":{"size":%d,"refill_time":1000},"ops":{"size":%d,"refill_time":1000}}`, bw, iops)
	}
	rootfsDrive += "}"
	if err = fcPut(ctx, client, "/drives/rootfs", rootfsDrive); err != nil {
		return info, fmt.Errorf("set drive: %w", err)
	}

	// track_dirty_pages is disabled — all snapshots are Full. This eliminates
	// Diff snapshot corruption (the rory incident) at the cost of ~500ms extra
	// per snapshot. With NVMe + btrfs this is negligible.
	// Disabling dirty page tracking also unlocks hugepages.
	hugePages := "None"
	if spec.Hugepages {
		hugePages = "2M"
	}
	if err = fcPut(ctx, client, "/machine-config", fmt.Sprintf(
		`{"vcpu_count":%d,"mem_size_mib":%d,"track_dirty_pages":false,"huge_pages":%q}`,
		vcpuCount, memMB, hugePages)); err != nil {
		return info, fmt.Errorf("set machine-config: %w", err)
	}

	// Entropy device — virtio-rng so guests don't block on getrandom().
	// 10 KB/s sustained, 8 KB initial burst for TLS handshakes / key generation.
	if err = fcPut(ctx, client, "/entropy",
		`{"rate_limiter":{"bandwidth":{"size":1024,"one_time_burst":8192,"refill_time":100}}}`); err != nil {
		slog.Warn("entropy device setup failed", "error", err) // non-fatal
	}

	// Balloon device — allows host to reclaim guest memory dynamically.
	// deflate_on_oom: guest reclaims memory when it needs it.
	// stats_polling_interval_s: FC collects balloon stats every 5s.
	// The thermal manager inflates the balloon on warm VMs to reclaim memory.
	if err = fcPut(ctx, client, "/balloon",
		`{"amount_mib":0,"deflate_on_oom":true,"stats_polling_interval_s":5}`); err != nil {
		slog.Warn("balloon device setup failed", "error", err) // non-fatal
	}

	if err = fcPut(ctx, client, "/vsock", fmt.Sprintf(
		`{"guest_cid":%d,"uds_path":%q}`, cid, vsockRef)); err != nil {
		return info, fmt.Errorf("set vsock: %w", err)
	}

	netPayload := fmt.Sprintf(`{"iface_id":"eth0","guest_mac":%q,"host_dev_name":%q`, mac, tapName)
	if bw := e.cfg.RateLimits.netBandwidth(); bw > 0 {
		burst := e.cfg.RateLimits.netBurst()
		netPayload += fmt.Sprintf(`,"rx_rate_limiter":{"bandwidth":{"size":%d,"one_time_burst":%d,"refill_time":1000}}`, bw, burst)
		netPayload += fmt.Sprintf(`,"tx_rate_limiter":{"bandwidth":{"size":%d,"one_time_burst":%d,"refill_time":1000}}`, bw, burst)
	}
	netPayload += "}"
	if err = fcPut(ctx, client, "/network-interfaces/eth0", netPayload); err != nil {
		return info, fmt.Errorf("set network: %w", err)
	}

	// 6b. Attach config drive as /dev/vdb
	if err = fcPut(ctx, client, "/drives/config", fmt.Sprintf(
		`{"drive_id":"config","path_on_host":%q,"is_root_device":false,"is_read_only":true}`,
		configRef)); err != nil {
		return info, fmt.Errorf("set config drive: %w", err)
	}

	// 6c. Attach persistent volume drives
	for _, vol := range spec.ResolvedVolumes {
		if err = fcPut(ctx, client, fmt.Sprintf("/drives/%s", vol.DriveID), fmt.Sprintf(
			`{"drive_id":%q,"path_on_host":%q,"is_root_device":false,"is_read_only":%v}`,
			vol.DriveID, volRefs[vol.DriveID], vol.ReadOnly)); err != nil {
			return info, fmt.Errorf("set persistent volume drive %s: %w", vol.DriveID, err)
		}
	}

	// 6d. Attach legacy ephemeral volume drives
	for i, vs := range spec.NewVolumes {
		driveID := fmt.Sprintf("ephvol%d", i)
		if err = fcPut(ctx, client, fmt.Sprintf("/drives/%s", driveID), fmt.Sprintf(
			`{"drive_id":%q,"path_on_host":%q,"is_root_device":false,"is_read_only":false}`,
			driveID, ephVolRefs[vs.Name])); err != nil {
			return info, fmt.Errorf("set volume drive %d: %w", i, err)
		}
	}

	// 7. Boot
	phase("instance_start")
	if err = fcPut(ctx, client, "/actions", `{"action_type":"InstanceStart"}`); err != nil {
		return info, fmt.Errorf("start instance: %w", err)
	}
	phase("instance_started")

	// 8. Wait for agent via TCP (kernel ip= already configured eth0).
	//
	// Pre-populate ARP with the guest's MAC so the first TCP SYN is sent
	// immediately without waiting for ARP resolution. Without this, the
	// host sends an ARP request that the guest can't answer yet (kernel
	// still booting), and Linux's ARP retransmit timer (retrans_time_ms
	// = 1000ms) adds a full second before the next ARP probe. We already
	// know the MAC — we generated it above.
	exec.Command("ip", "neigh", "replace", guestIP, "lladdr", mac,
		"dev", userNet.BridgeName, "nud", "permanent").Run() // best-effort

	phase("wait_ready_start")
	agentClient := agent.NewTCPClientWithAuth(guestIP, token)
	if err = agentClient.WaitReady(ctx, 30*time.Second); err != nil {
		return info, fmt.Errorf("agent not ready: %w", err)
	}
	phase("wait_ready_done")

	vm := &VM{
		ID: id, Name: name, UserID: spec.UserID,
		SocketPath: apiSocket,
		VsockPath: vsockPath, RootfsPath: rootfsPath,
		CID: cid, VcpuCount: vcpuCount, MemSizeMib: memMB,
		TapDevice: tapName, GuestIP: guestIP, GuestMAC: mac,
		Token: token, FCPathOrigin: id, Volumes: volAttachments,
		Agent: agentClient, Status: "running",
		Thermal: "hot", cancel: vmCancel, cmd: fcCmd,
		stderrBuf: stderrBuf, jailRoot: fcProc.jailRoot,
	}

	phase("mu_lock_wait")
	e.mu.Lock()
	e.vms[id] = vm
	e.mu.Unlock()
	phase("create_complete")

	return engine.SandboxInfo{
		ID: id, Name: name, Status: "running",
		IP: guestIP, EngineID: id,
	}, nil
}

