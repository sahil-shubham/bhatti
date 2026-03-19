package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/sahil-shubham/bhatti/pkg"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/engine/docker"
	"github.com/sahil-shubham/bhatti/pkg/server"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

func main() {
	// CLI mode: any subcommand other than "serve" (or no args)
	if len(os.Args) > 1 && os.Args[1] != "serve" {
		runCLI()
		return
	}
	runDaemon()
}

func runDaemon() {
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
	case "firecracker":
		eng, err = newFirecrackerEngine(cfg)
	case "docker", "":
		eng, err = docker.New()
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

	// Find web directory
	webDir := "web"
	if _, err := os.Stat(webDir); os.IsNotExist(err) {
		// Try relative to executable
		if ex, err := os.Executable(); err == nil {
			webDir = filepath.Join(filepath.Dir(ex), "web")
		}
	}

	// Start server
	srv := server.New(eng, st, cfg.AuthToken, webDir)

	// Resolve the port for display
	port := cfg.Listen

	slog.Info("bhatti listening", "addr", cfg.Listen)
	if lanIP := getLanIP(); lanIP != "" {
		slog.Info("endpoints",
			"local", "http://localhost"+port,
			"network", "http://"+lanIP+port,
		)
	}

	// Graceful shutdown: clean up TAP devices on SIGTERM/SIGINT
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		slog.Info("shutting down", "signal", sig)
		if shutdowner, ok := eng.(interface{ Shutdown() }); ok {
			shutdowner.Shutdown()
		}
		os.Exit(0)
	}()

	if err := http.ListenAndServe(cfg.Listen, srv); err != nil {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}

// recoverVMs restores Firecracker VMs from the SQLite store on startup.
func recoverVMs(st *store.Store, provider engine.VMStateProvider) {
	sandboxes, err := st.ListSandboxes()
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
		if err != nil || fcState.RootfsPath == "" {
			continue // Not a Firecracker sandbox
		}

		state := map[string]interface{}{
			"rootfs_path":   fcState.RootfsPath,
			"snap_mem_path": fcState.SnapMemPath,
			"snap_vm_path":  fcState.SnapVMPath,
			"vsock_cid":     fcState.VsockCID,
			"tap_device":    fcState.TapDevice,
			"guest_ip":      fcState.GuestIP,
			"guest_mac":     fcState.GuestMAC,
			"vcpu_count":    fcState.VcpuCount,
			"mem_size_mib":  fcState.MemSizeMib,
			"socket_path":   fcState.SocketPath,
			"vsock_path":    fcState.VsockPath,
		}

		if sb.Status == "stopped" && fcState.SnapMemPath != "" {
			if _, err := os.Stat(fcState.SnapMemPath); err == nil {
				provider.RestoreVM(sb.EngineID, sb.Name, "stopped", state)
				recovered++
				slog.Info("recovered sandbox", "name", sb.Name, "id", sb.ID, "status", "stopped")
			} else {
				st.UpdateSandboxStatus(sb.ID, "unknown")
				slog.Warn("snapshot missing", "name", sb.Name, "id", sb.ID)
			}
		} else if sb.Status == "running" {
			if fcState.SnapMemPath != "" {
				st.UpdateSandboxStatus(sb.ID, "stopped")
				provider.RestoreVM(sb.EngineID, sb.Name, "stopped", state)
				recovered++
				slog.Info("recovered sandbox", "name", sb.Name, "id", sb.ID, "status", "stopped (was running)")
			} else {
				st.UpdateSandboxStatus(sb.ID, "unknown")
				slog.Warn("sandbox was running with no snapshot", "name", sb.Name, "id", sb.ID)
			}
		}
	}

	if recovered > 0 {
		slog.Info("recovery complete", "count", recovered)
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
