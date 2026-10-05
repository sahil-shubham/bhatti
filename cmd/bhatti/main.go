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
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/acme/autocert"

	"github.com/sahil-shubham/bhatti/pkg"
	"github.com/sahil-shubham/bhatti/pkg/backup"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/engine/krucible"
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
	// Structured JSON logging for production.
	// Log level configurable via BHATTI_LOG_LEVEL env var (debug, info, warn, error).
	logLevel := slog.LevelInfo
	switch os.Getenv("BHATTI_LOG_LEVEL") {
	case "debug", "DEBUG":
		logLevel = slog.LevelDebug
	case "warn", "WARN":
		logLevel = slog.LevelWarn
	case "error", "ERROR":
		logLevel = slog.LevelError
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	cfg, err := pkg.LoadConfig()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	if cfg.ConfigPath != "" {
		slog.Info("config loaded", "path", cfg.ConfigPath)
	} else {
		slog.Info("no config file found, using defaults")
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
	case "krucible", "":
		// Before krucible adopts helpers: older data dirs name tier images by
		// path, which the next image update would overwrite.
		migrateImages(cfg.DataDir)
		eng, err = newKrucibleEngine(cfg)
	case "firecracker":
		slog.Error("the firecracker engine was removed in v2 (krucible). For Firecracker, use the `firecracker` branch / a v1.x release; otherwise set engine: krucible")
		os.Exit(1)
	default:
		slog.Error("unknown engine", "engine", cfg.Engine)
		os.Exit(1)
	}
	if err != nil {
		// Include which config was actually loaded: the most common cause of
		// "set BaseImage or BaseRootfs" is the daemon silently reading a
		// CLI-only config (no krucible_* keys) instead of the server config.
		slog.Error("create engine", "error", err,
			"config", cfg.ConfigPath,
			"hint", "a server config needs the krucible_* keys; select an explicit file with BHATTI_CONFIG=/path/to/config.yaml")
		os.Exit(1)
	}

	// krucible recovers its own VMs internally: New() adopts the helpers the
	// previous daemon left running (its shutdown stops none) and marks the rest
	// stopped; srv.RecoverSandboxes below brings the store in line. There are no
	// host TAP devices to reclaim (netd is userspace).

	// Detach volumes orphaned by crashed sandboxes after krucible has
	// reconciled its helpers in New.
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

	// After adoption and the temp-dir cleanup above, before serving: nothing
	// can be naming a base it is about to remove.
	gcImages(cfg)

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
	if lp, ok := eng.(interface{ LoharPath() string }); ok {
		srvOpts = append(srvOpts, server.WithLoharPath(lp.LoharPath()))
	}
	switch cfg.DefaultEgress {
	case "", "none", "deny", "public":
		srvOpts = append(srvOpts, server.WithDefaultEgress(cfg.DefaultEgress))
	default:
		slog.Error("invalid default_egress (want none, deny or public)", "value", cfg.DefaultEgress)
		os.Exit(1)
	}
	srvOpts = append(srvOpts, server.WithMountRoots(cfg.MountRoots, cfg.ConfigPaths))
	if cfg.Domain != nil {
		srvOpts = append(srvOpts,
			server.WithProxyZone(cfg.Domain.ProxyZone),
			server.WithAPIHost(cfg.Domain.APIHost),
		)
	}
	srv := server.New(eng, st, cfg.DataDir, srvOpts...)

	// Start observability: event recorder, metrics snapshots, retention
	srv.StartEventRecorder()
	// Before the thermal manager's first cycle, and after the recorder.
	srv.RecoverSandboxes(context.Background())
	srv.StartRetention()
	// After the recorder: the broker audits every credential use and refusal.
	srv.StartCredentialBroker()

	// Start thermal manager to transition idle VMs: hot → warm → cold
	if err := srv.StartThermalManager(server.ThermalConfig{
		WarmTimeout: 30 * time.Second, // hot → warm after 30s idle
		ColdTimeout: 30 * time.Minute, // warm → cold after 30min idle
	}); err != nil {
		slog.Error("start thermal manager", "error", err)
		os.Exit(1)
	}

	// Start metrics snapshots after thermal manager and public proxy are
	// wired up (so the snapshot goroutine can read proxy counters).
	// Deferred to after startDomainMode/startPlainMode which sets publicProxy.

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

	// Start metrics snapshots now that public proxy is wired up.
	srv.StartMetricsSnapshots()

	// Record daemon.started event
	recoveredCount := 0
	if allSandboxes, err := st.ListAllSandboxes(); err == nil {
		for _, sb := range allSandboxes {
			if sb.Status == "running" || sb.Status == "stopped" {
				recoveredCount++
			}
		}
	}
	srv.RecordEvent(store.Event{
		Type: "daemon.started",
		Meta: map[string]any{
			"version":       version,
			"recovered_vms": recoveredCount,
			"listen":        cfg.Listen,
		},
	})

	// Auto-wake keep_hot sandboxes after recovery. These sandboxes maintain
	// persistent external connections that die on pause. One that outlived the
	// previous daemon is hot already (a no-op here); one whose VM died while no
	// daemon was running, or that was stopped, boots now.
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

	// Sandboxes outlive the daemon: Shutdown stops none of them, and the next
	// daemon adopts them as they are.
	srv.Shutdown(sig.String())

	slog.Info("shutdown complete")
}

// serveControlSocket serves the full control mux on a unix socket — the local
// CLI channel, never reachable from a sandbox (unlike a loopback TCP port, which
// TSI proxies through). Returned so the caller can add it to the shutdown set.
func serveControlSocket(cfg *pkg.Config, srv *server.Server) *http.Server {
	sock := cfg.APISocketPath()
	_ = os.MkdirAll(filepath.Dir(sock), 0700)
	_ = os.Remove(sock) // clear a stale socket from a prior run
	ln, err := net.Listen("unix", sock)
	if err != nil {
		slog.Error("control socket listen", "path", sock, "error", err)
		os.Exit(1)
	}
	_ = os.Chmod(sock, 0600) // owner-only
	s := &http.Server{Handler: srv}
	go func() {
		slog.Info("control API listening", "socket", sock)
		if err := s.Serve(ln); err != http.ErrServerClosed {
			slog.Error("control socket server failed", "error", err)
		}
	}()
	return s
}

// startPlainMode serves the control API on the unix socket (always) and,
// optionally, on cfg.Listen (TCP) for dev/remote; empty Listen = socket only.
func startPlainMode(cfg *pkg.Config, eng engine.Engine, st *store.Store, srv *server.Server) []*http.Server {
	servers := []*http.Server{serveControlSocket(cfg, srv)}

	if cfg.Listen != "" {
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
	}

	// Optional path-based public proxy (dev/testing)
	if cfg.PublicProxyListen != "" {
		pubHandler := server.NewPublicProxyHandler(eng, st, srv.ResumeSem(), func(engineID string) {
			srv.TouchActivity(engineID)
		}, func(ctx context.Context, engineID string) error {
			return srv.EnsureHot(ctx, engineID)
		})
		pubHandler.SetRecordEvent(func(e store.Event) { srv.RecordEvent(e) })
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
	}, func(ctx context.Context, engineID string) error {
		return srv.EnsureHot(ctx, engineID)
	})
	pubHandler.SetRecordEvent(func(e store.Event) { srv.RecordEvent(e) })
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
		// Strip "h2" from autocert's NextProtos — we disable HTTP/2 via
		// TLSNextProto (below), but if the TLS layer still advertises h2
		// in ALPN, Cloudflare will negotiate it and then send h2 frames
		// that Go's HTTP/1.1 parser cannot read → broken connections.
		filtered := tlsConfig.NextProtos[:0]
		for _, p := range tlsConfig.NextProtos {
			if p != "h2" {
				filtered = append(filtered, p)
			}
		}
		tlsConfig.NextProtos = filtered
		httpHandler = cm.HTTPHandler(http.HandlerFunc(redirectHTTPS))
	} else {
		slog.Error("domain mode requires tls_cert+tls_key or acme_email")
		os.Exit(1)
	}

	// :443 — serves both API (api.bhatti.sh) and proxy (*.bhatti.sh)
	//
	// Disable HTTP/2 on the origin. Cloudflare's "HTTP/2 to Origin" (enabled
	// by default) negotiates h2 via ALPN when the origin advertises it.
	// gorilla/websocket (and Go's net/http) cannot perform the HTTP/1.1
	// 101 Switching Protocols upgrade over an HTTP/2 connection (RFC 8441
	// extended CONNECT is not yet supported), so WebSocket endpoints
	// (/_shell/*/ws, sandbox exec, proxy) silently fail with 400.
	//
	// Setting TLSNextProto to an empty map is the standard Go idiom to
	// disable HTTP/2.  Cloudflare still serves HTTP/2 to end-user clients;
	// only the Cloudflare↔origin hop drops to HTTP/1.1.
	//
	// See: gorilla/websocket#663, caddyserver/caddy#7292, golang/go#49918
	httpsServer := &http.Server{
		Addr:              ":443",
		Handler:           srv,
		TLSConfig:         tlsConfig,
		TLSNextProto:      make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
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

	// Internal API on a unix socket (was 127.0.0.1:8080, which TSI let sandboxes
	// reach — the vulnerability that triggered the v2 networking rewrite).
	servers = append(servers, serveControlSocket(cfg, srv))

	return servers
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
			exists, err := st.PersistentVolumeExists(userID, volName)
			if err != nil {
				slog.Warn("skipping volume file cleanup: database lookup failed", "user_id", userID, "volume", volName, "error", err)
				continue
			}
			if !exists {
				orphanPath := filepath.Join(userPath, f.Name())
				slog.Info("removing orphaned volume file", "path", orphanPath)
				if err := os.Remove(orphanPath); err != nil {
					slog.Warn("remove orphaned volume file", "path", orphanPath, "error", err)
				}
			}
		}
	}
}

// registerTierImages discovers rootfs tier images on disk and registers them
// as admin images (user_id=”) so users can reference them with --image.
// Tiers are discovered by globbing for rootfs-*-{arch}.ext4 in the images
// directory — no hardcoded list. Adding a new tier only requires the file
// to exist on disk (placed there by install.sh).
func registerTierImages(cfg *pkg.Config, st *store.Store) {
	// Guest arch == host arch (HVF and KVM both run native-arch guests), so the
	// binary's own GOARCH is the truth. (Previously sniffed /proc/cpuinfo,
	// which doesn't exist on macOS — it only worked there by fallback accident.)
	arch := runtime.GOARCH

	pattern := filepath.Join(cfg.DataDir, "images", fmt.Sprintf("rootfs-*-%s.ext4", arch))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		slog.Warn("failed to glob tier images", "pattern", pattern, "error", err)
		return
	}

	suffix := fmt.Sprintf("-%s.ext4", arch)
	for _, path := range matches {
		// Extract tier name: rootfs-browser-arm64.ext4 → browser
		base := filepath.Base(path)
		tier := strings.TrimPrefix(base, "rootfs-")
		tier = strings.TrimSuffix(tier, suffix)
		if tier == "" || tier == base {
			continue
		}

		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			continue
		}
		// Check if already registered
		if _, err := st.GetImage("", tier); err == nil {
			continue // already exists
		}
		st.CreateImage(store.ImageRecord{
			ID:        fmt.Sprintf("tier_%s_%s", tier, arch),
			UserID:    "", // admin image, visible to all
			Name:      tier,
			Source:    "built-in",
			FilePath:  path,
			SizeMB:    int(info.Size() / 1024 / 1024),
			CreatedAt: info.ModTime(),
		})
		slog.Info("registered tier image", "name", tier, "path", path)
	}
}

// migrateImages brings the data dir to the immutable-base layout
// (pkg/engine/krucible/bases.go). Problems are logged, not fatal: what was
// left alone still works; the installer refuses to switch a tier while
// anything is left.
func migrateImages(dataDir string) {
	m, err := krucible.MigrateImages(dataDir)
	for _, s := range m.Moved {
		slog.Info("images.migrate.tier", "moved", s)
	}
	if len(m.Rewritten) > 0 {
		slog.Info("images.migrate.disks", "repointed", len(m.Rewritten))
	}
	for _, s := range m.Broken {
		slog.Warn("images.migrate.broken", "disk", s)
	}
	for _, s := range m.Problems {
		slog.Error("images.migrate.problem", "detail", s)
	}
	if err != nil {
		slog.Error("images.migrate", "error", err)
	}
}

// gcImages removes base images nothing uses any more.
func gcImages(cfg *pkg.Config) {
	removed, err := krucible.GCImages(cfg.DataDir, []string{cfg.KrucibleBaseImage}, false)
	for _, p := range removed {
		slog.Info("images.gc.removed", "base", p)
	}
	if err != nil {
		slog.Error("images.gc", "error", err)
	}
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
