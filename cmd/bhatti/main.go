package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/acme/autocert"

	"github.com/sahil-shubham/bhatti/pkg"
	"github.com/sahil-shubham/bhatti/pkg/backup"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/server"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// version is set at build time via -ldflags
var version = "dev"

func main() {
	// Propagate build-time version to server package for X-Bhatti-Version header.
	server.ServerVersion = version

	// Register the serve command here (not in cli.go) because it imports
	// the engine packages which have Linux build tags.
	serveCmd := &cobra.Command{
		Use:     "serve",
		Short:   "Start the bhatti daemon",
		GroupID: "admin",
		Run: func(cmd *cobra.Command, args []string) {
			runDaemon()
		},
	}
	rootCmd.AddCommand(serveCmd)

	runCLI()
}

func runDaemon() {
	// Structured JSON logging for production
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, err := pkg.LoadConfig()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	// Ensure data directory
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		slog.Error("create data dir", "error", err)
		os.Exit(1)
	}

	// Generate SSH keypair
	keyPath, err := pkg.EnsureKeypair(cfg.DataDir)
	if err != nil {
		slog.Error("ensure keypair", "error", err)
		os.Exit(1)
	}
	slog.Info("SSH key ready", "path", keyPath)

	// Open store
	st, err := store.New(filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		slog.Error("open store", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	// Create engine
	var eng engine.Engine
	switch cfg.Engine {
	case "firecracker", "":
		eng, err = newFirecrackerEngine(cfg)
	default:
		slog.Error("unknown engine", "engine", cfg.Engine)
		os.Exit(1)
	}
	if err != nil {
		slog.Error("create engine", "error", err)
		os.Exit(1)
	}

	// Recover Firecracker VMs from store if applicable
	if provider, ok := eng.(engine.VMStateProvider); ok {
		recoverVMs(st, provider)
	}

	// Clean up orphaned TAP devices AFTER recovery so that restored VMs'
	// TAPs are preserved. Before this point, all TAPs look like orphans.
	if cleaner, ok := eng.(interface{ CleanupOrphanedTaps() }); ok {
		cleaner.CleanupOrphanedTaps()
	}

	// v0.3: Detach volumes orphaned by crashed sandboxes.
	// MUST run after recoverVMs (which marks dead-process sandboxes as stopped/unknown).
	if n, err := st.DetachOrphanedPersistentVolumes(); err != nil {
		slog.Warn("orphan volume detach failed", "error", err)
	} else if n > 0 {
		slog.Info("detached orphaned volume attachments", "count", n)
	}

	// v0.3: Reconcile orphaned volume files on disk.
	// If daemon crashed between store.DeletePersistentVolume (removes DB row)
	// and os.Remove (removes .ext4 file), the file lingers with no store record.
	reconcileOrphanedVolumeFiles(cfg.DataDir, st)

	// v0.3: Clean stale checkpoint temp dirs left by crashed checkpoints
	snapshotDir := filepath.Join(cfg.DataDir, "snapshots")
	if entries, err := os.ReadDir(snapshotDir); err == nil {
		for _, userDir := range entries {
			if !userDir.IsDir() {
				continue
			}
			userPath := filepath.Join(snapshotDir, userDir.Name())
			subEntries, _ := os.ReadDir(userPath)
			for _, entry := range subEntries {
				if entry.IsDir() && strings.HasSuffix(entry.Name(), ".tmp") {
					tmpPath := filepath.Join(userPath, entry.Name())
					slog.Info("removing stale snapshot temp dir", "path", tmpPath)
					os.RemoveAll(tmpPath)
				}
			}
		}
	}

	// Register tier rootfs images as system images so --image browser/minimal/docker works.
	// Uses user_id='' (admin images visible to all users). Idempotent — skips if already exists.
	registerTierImages(cfg, st)

	// v0.4: Clean orphaned publish rules
	if n, err := st.CleanupOrphanedPublishRules(); err != nil {
		slog.Warn("orphaned publish rule cleanup failed", "error", err)
	} else if n > 0 {
		slog.Info("cleaned up orphaned publish rules", "count", n)
	}

	// Start server
	var srvOpts []server.ServerOption

	// Configure backup backend if S3 is configured
	if cfg.Backup != nil && cfg.Backup.S3Endpoint != "" {
		srvOpts = append(srvOpts, server.WithBackupBackend(backup.NewS3(backup.S3Config{
			Endpoint:  cfg.Backup.S3Endpoint,
			Region:    cfg.Backup.S3Region,
			Bucket:    cfg.Backup.S3Bucket,
			AccessKey: cfg.Backup.S3AccessKey,
			SecretKey: cfg.Backup.S3SecretKey,
		})))
		slog.Info("backup configured", "endpoint", cfg.Backup.S3Endpoint, "bucket", cfg.Backup.S3Bucket)
	}
	if cfg.PublicProxyListen != "" {
		srvOpts = append(srvOpts, server.WithPublicProxyAddr(cfg.PublicProxyListen))
	}
	if cfg.Domain != nil {
		srvOpts = append(srvOpts,
			server.WithProxyZone(cfg.Domain.ProxyZone),
			server.WithAPIHost(cfg.Domain.APIHost),
		)
	}
	srv := server.New(eng, st, cfg.DataDir, srvOpts...)

	// Start thermal manager to transition idle VMs: hot → warm → cold
	srv.StartThermalManager(server.ThermalConfig{
		WarmTimeout: 30 * time.Second,  // hot → warm after 30s idle
		ColdTimeout: 30 * time.Minute,  // warm → cold after 30min idle
	})

	// Start scheduled backup goroutine if configured
	if cfg.Backup != nil && len(cfg.Backup.Schedule) > 0 {
		srv.StartBackupScheduler(cfg.Backup.Schedule)
	}

	var servers []*http.Server
	if cfg.Domain != nil {
		servers = startDomainMode(cfg, eng, st, srv)
	} else {
		servers = startPlainMode(cfg, eng, st, srv)
	}

	// Auto-wake keep_hot sandboxes after recovery. These sandboxes maintain
	// persistent external connections that die on pause — leaving them cold
	// after a daemon restart defeats the purpose of keep_hot.
	go func() {
		hotSandboxes, err := st.ListAllSandboxes()
		if err != nil {
			slog.Warn("auto-wake: list sandboxes", "error", err)
			return
		}
		for _, sb := range hotSandboxes {
			if !sb.KeepHot || sb.Status == "destroyed" {
				continue
			}
			wakeCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			if err := srv.EnsureHot(wakeCtx, sb.EngineID); err != nil {
				slog.Error("auto-wake failed",
					"sandbox", sb.Name, "id", sb.ID, "error", err)
			} else {
				st.UpdateSandboxStatus(sb.ID, "running")
				slog.Info("auto-wake: sandbox started",
					"sandbox", sb.Name, "id", sb.ID)
			}
			cancel()
		}
	}()

	// Wait for SIGTERM/SIGINT
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("shutting down", "signal", sig)

	// Drain HTTP connections (30s timeout for safety)
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()
	for _, s := range servers {
		s.Shutdown(shutCtx)
	}

	// Stop background goroutines (thermal manager, task cleanup)
	srv.Close()

	// Snapshot all running VMs before killing the engine.
	// This ensures every hot/warm sandbox has a snapshot on disk
	// so recoverVMs can restore them on the next startup.
	srv.SnapshotAll()

	// Stop engine (kill VMs, clean TAPs)
	if shutdowner, ok := eng.(interface{ Shutdown() }); ok {
		shutdowner.Shutdown()
	}

	slog.Info("shutdown complete")
}

// startPlainMode starts the API on :8080 + optional path-based public proxy.
func startPlainMode(cfg *pkg.Config, eng engine.Engine, st *store.Store, srv *server.Server) []*http.Server {
	var servers []*http.Server

	httpServer := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv,
	}
	servers = append(servers, httpServer)

	port := cfg.Listen
	go func() {
		slog.Info("bhatti listening", "addr", cfg.Listen)
		if lanIP := getLanIP(); lanIP != "" {
			slog.Info("endpoints",
				"local", "http://localhost"+port,
				"network", "http://"+lanIP+port,
			)
		}
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	// Optional path-based public proxy (dev/testing)
	if cfg.PublicProxyListen != "" {
		pubHandler := server.NewPublicProxyHandler(eng, st, srv.ResumeSem(), func(engineID string) {
			srv.TouchActivity(engineID)
		})
		pubServer := &http.Server{
			Addr:         cfg.PublicProxyListen,
			Handler:      http.HandlerFunc(pubHandler.ServeHTTPPathBased),
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 60 * time.Second,
			IdleTimeout:  120 * time.Second,
		}
		servers = append(servers, pubServer)
		go func() {
			slog.Info("public proxy listening", "addr", cfg.PublicProxyListen)
			if err := pubServer.ListenAndServe(); err != http.ErrServerClosed {
				slog.Error("public proxy failed", "error", err)
			}
		}()
	}

	return servers
}

// redirectHTTPS redirects HTTP to HTTPS.
func redirectHTTPS(w http.ResponseWriter, r *http.Request) {
	target := "https://" + r.Host + r.URL.RequestURI()
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// startDomainMode starts :443 + :80 (redirect) + 127.0.0.1:8080 (internal).
func startDomainMode(cfg *pkg.Config, eng engine.Engine, st *store.Store, srv *server.Server) []*http.Server {
	dom := cfg.Domain
	var servers []*http.Server

	// Create public proxy handler and attach to server for host-based routing
	pubHandler := server.NewPublicProxyHandler(eng, st, srv.ResumeSem(), func(engineID string) {
		srv.TouchActivity(engineID)
	})
	srv.SetPublicProxy(pubHandler)

	// TLS config
	var tlsConfig *tls.Config
	var httpHandler http.Handler // :80 handler

	if dom.TLSCert != "" && dom.TLSKey != "" {
		// Option A: Bring your own (wildcard) cert
		cert, err := tls.LoadX509KeyPair(dom.TLSCert, dom.TLSKey)
		if err != nil {
			slog.Error("load TLS cert", "error", err)
			os.Exit(1)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
		httpHandler = http.HandlerFunc(redirectHTTPS)
	} else if dom.ACMEEmail != "" {
		// Option B: Per-alias autocert
		slog.Warn("per-alias TLS is rate-limited to 50 new aliases/week — for preview environments, use a wildcard cert (tls_cert/tls_key)")
		certDir := filepath.Join(cfg.DataDir, "certs")
		os.MkdirAll(certDir, 0700)
		cm := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: srv.HostPolicy,
			Cache:      autocert.DirCache(certDir),
			Email:      dom.ACMEEmail,
		}
		tlsConfig = cm.TLSConfig()
		httpHandler = cm.HTTPHandler(http.HandlerFunc(redirectHTTPS))
	} else {
		slog.Error("domain mode requires tls_cert+tls_key or acme_email")
		os.Exit(1)
	}

	// :443 — serves both API (api.bhatti.sh) and proxy (*.bhatti.sh)
	httpsServer := &http.Server{
		Addr:              ":443",
		Handler:           srv,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	servers = append(servers, httpsServer)
	go func() {
		slog.Info("bhatti listening (domain mode)",
			"api", "https://"+dom.APIHost,
			"proxy", "https://*."+dom.ProxyZone,
		)
		if err := httpsServer.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
			slog.Error("HTTPS server failed", "error", err)
			os.Exit(1)
		}
	}()

	// :80 — ACME challenges + HTTPS redirect
	httpServer := &http.Server{
		Addr:    ":80",
		Handler: httpHandler,
	}
	servers = append(servers, httpServer)
	go func() {
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			slog.Error("HTTP redirect server failed", "error", err)
		}
	}()

	// 127.0.0.1:8080 — internal API (health checks, monitoring, local tools)
	loopback := &http.Server{
		Addr:    "127.0.0.1:8080",
		Handler: srv,
	}
	servers = append(servers, loopback)
	go func() {
		slog.Info("internal API listening", "addr", "127.0.0.1:8080")
		if err := loopback.ListenAndServe(); err != http.ErrServerClosed {
			slog.Error("loopback server failed", "error", err)
		}
	}()

	return servers
}

// recoverVMs restores Firecracker VMs from the SQLite store on startup.
func recoverVMs(st *store.Store, provider engine.VMStateProvider) {
	sandboxes, err := st.ListAllSandboxes()
	if err != nil {
		slog.Warn("recovery: list sandboxes", "error", err)
		return
	}

	recovered := 0
	for _, sb := range sandboxes {
		if sb.Status == "destroyed" {
			continue
		}

		fcState, err := st.LoadFirecrackerState(sb.ID)
		if err != nil {
			continue // Not a Firecracker sandbox (no FC state row)
		}
		if fcState.RootfsPath == "" {
			// Has an FC state row but no rootfs — corrupted or partially created.
			// Mark as unknown so the user can see something went wrong.
			if sb.Status == "running" || sb.Status == "stopped" {
				st.UpdateSandboxStatus(sb.ID, "unknown")
				slog.Warn("sandbox has no rootfs path", "name", sb.Name, "id", sb.ID)
			}
			continue
		}

		// Look up user's subnet index for network recovery
		var subnetIndex int
		if sb.CreatedBy != "" {
			if user, err := st.GetUser(sb.CreatedBy); err == nil {
				subnetIndex = user.SubnetIndex
			}
		}

		state := map[string]interface{}{
			"rootfs_path":       fcState.RootfsPath,
			"snap_mem_path":     fcState.SnapMemPath,
			"snap_vm_path":      fcState.SnapVMPath,
			"vsock_cid":         fcState.VsockCID,
			"tap_device":        fcState.TapDevice,
			"guest_ip":          fcState.GuestIP,
			"guest_mac":         fcState.GuestMAC,
			"vcpu_count":        fcState.VcpuCount,
			"mem_size_mib":      fcState.MemSizeMib,
			"socket_path":       fcState.SocketPath,
			"vsock_path":        fcState.VsockPath,
			"user_id":           sb.CreatedBy,
			"subnet_index":      subnetIndex,
			"agent_token":       fcState.AgentToken,
			"has_base_snapshot": fcState.HasBaseSnapshot,
			"fc_path_origin":    fcState.FCPathOrigin,
		}

		// Verify all critical snapshot files exist and are non-empty.
		// Checking only SnapMemPath misses corrupt/truncated vm.snap or rootfs.
		snapshotOK := fcState.SnapMemPath != "" && fcState.SnapVMPath != "" &&
			fileExistsAndNonEmpty(fcState.SnapMemPath) &&
			fileExistsAndNonEmpty(fcState.SnapVMPath) &&
			fileExistsAndNonEmpty(fcState.RootfsPath)

		if sb.Status == "stopped" && snapshotOK {
			provider.RestoreVM(sb.EngineID, sb.Name, "stopped", state)
			recovered++
			slog.Info("recovered sandbox", "name", sb.Name, "id", sb.ID, "status", "stopped")
		} else if sb.Status == "stopped" {
			st.UpdateSandboxStatus(sb.ID, "unknown")
			slog.Warn("snapshot files missing or corrupt", "name", sb.Name, "id", sb.ID,
				"mem", fcState.SnapMemPath, "vm", fcState.SnapVMPath, "rootfs", fcState.RootfsPath)
		} else if sb.Status == "running" {
			if snapshotOK {
				st.UpdateSandboxStatus(sb.ID, "stopped")
				provider.RestoreVM(sb.EngineID, sb.Name, "stopped", state)
				recovered++
				slog.Info("recovered sandbox", "name", sb.Name, "id", sb.ID, "status", "stopped (was running)")
			} else {
				st.UpdateSandboxStatus(sb.ID, "unknown")
				slog.Warn("sandbox was running with no valid snapshot", "name", sb.Name, "id", sb.ID)
			}
		}
	}

	if recovered > 0 {
		slog.Info("recovery complete", "count", recovered)
	}
}

// reconcileOrphanedVolumeFiles walks the volumes directory and removes
// .ext4 files that have no matching store record. This handles the crash
// window between store.DeletePersistentVolume and os.Remove.
func reconcileOrphanedVolumeFiles(dataDir string, st *store.Store) {
	volRoot := filepath.Join(dataDir, "volumes")
	userDirs, err := os.ReadDir(volRoot)
	if err != nil {
		return // volumes dir may not exist yet
	}
	for _, userDir := range userDirs {
		if !userDir.IsDir() {
			continue
		}
		userID := userDir.Name()
		userPath := filepath.Join(volRoot, userID)
		files, err := os.ReadDir(userPath)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".ext4") {
				continue
			}
			volName := strings.TrimSuffix(f.Name(), ".ext4")
			if _, err := st.GetPersistentVolume(userID, volName); err != nil {
				orphanPath := filepath.Join(userPath, f.Name())
				slog.Info("removing orphaned volume file", "path", orphanPath)
				os.Remove(orphanPath)
			}
		}
	}
}

// registerTierImages ensures the built-in rootfs tiers (minimal, browser, docker)
// are registered in the images table as admin images (user_id=''). This allows
// users to specify --image browser or --image minimal on create.
func registerTierImages(cfg *pkg.Config, st *store.Store) {
	// Auto-detect architecture
	arch := "arm64"
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		s := string(data)
		if strings.Contains(s, "GenuineIntel") || strings.Contains(s, "AuthenticAMD") {
			arch = "amd64"
		}
	}

	tiers := []string{"minimal", "browser", "docker"}
	for _, tier := range tiers {
		path := filepath.Join(cfg.DataDir, "images", fmt.Sprintf("rootfs-%s-%s.ext4", tier, arch))
		info, err := os.Stat(path)
		if err != nil {
			continue // tier not installed
		}
		// Check if already registered
		if _, err := st.GetImage("", tier); err == nil {
			continue // already exists
		}
		st.CreateImage(store.ImageRecord{
			ID:       fmt.Sprintf("tier_%s_%s", tier, arch),
			UserID:   "", // admin image, visible to all
			Name:     tier,
			Source:   "built-in",
			FilePath: path,
			SizeMB:   int(info.Size() / 1024 / 1024),
			CreatedAt: info.ModTime(),
		})
		slog.Info("registered tier image", "name", tier, "path", path)
	}
}

// fileExistsAndNonEmpty returns true if the path exists and has size > 0.
// Used by recovery to detect truncated/corrupt snapshot files.
func fileExistsAndNonEmpty(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fi.Size() > 0
}

// getLanIP returns the first non-loopback IPv4 address, or "" if none found.
func getLanIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				return fmt.Sprintf("%s", ip4)
			}
		}
	}
	return ""
}
