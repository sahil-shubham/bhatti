package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sahil-shubham/bhatti/pkg"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/server"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// version is set at build time via -ldflags
var version = "dev"

func main() {
	// Register the serve command here (not in cli.go) because it imports
	// the engine packages which have Linux build tags.
	rootCmd.AddCommand(&cobra.Command{
		Use:   "serve",
		Short: "Start the bhatti daemon",
		Run: func(cmd *cobra.Command, args []string) {
			runDaemon()
		},
	})

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

	// Start server
	srv := server.New(eng, st, cfg.DataDir)

	// Resolve the port for display
	port := cfg.Listen

	httpServer := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv,
	}

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

	// Wait for SIGTERM/SIGINT
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("shutting down", "signal", sig)

	// Drain HTTP connections (5s timeout)
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	httpServer.Shutdown(shutCtx)

	// Stop background goroutines (port scanner, thermal manager)
	srv.Close()

	// Stop engine (kill VMs, clean TAPs)
	if shutdowner, ok := eng.(interface{ Shutdown() }); ok {
		shutdowner.Shutdown()
	}

	slog.Info("shutdown complete")
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
		if err != nil || fcState.RootfsPath == "" {
			continue // Not a Firecracker sandbox
		}

		// Look up user's subnet index for network recovery
		var subnetIndex int
		if sb.CreatedBy != "" {
			if user, err := st.GetUser(sb.CreatedBy); err == nil {
				subnetIndex = user.SubnetIndex
			}
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
			"user_id":       sb.CreatedBy,
			"subnet_index":  subnetIndex,
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
