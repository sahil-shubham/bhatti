package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sahilshubham/bhatti/pkg"
	"github.com/sahilshubham/bhatti/pkg/engine"
	"github.com/sahilshubham/bhatti/pkg/engine/docker"
	"github.com/sahilshubham/bhatti/pkg/server"
	"github.com/sahilshubham/bhatti/pkg/store"
)

func main() {
	cfg, err := pkg.LoadConfig()
	if err != nil {
		log.Fatal(err)
	}

	// Ensure data directory
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		log.Fatal(err)
	}

	// Generate SSH keypair
	keyPath, err := pkg.EnsureKeypair(cfg.DataDir)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("SSH key: %s", keyPath)

	// Open store
	st, err := store.New(filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		log.Fatal(err)
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
		log.Fatalf("unknown engine: %s", cfg.Engine)
	}
	if err != nil {
		log.Fatal(err)
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
	if strings.HasPrefix(port, ":") {
		port = port // already just ":PORT"
	}

	log.Printf("bhatti listening on %s", cfg.Listen)
	if lanIP := getLanIP(); lanIP != "" {
		log.Printf("  → local:   http://localhost%s", port)
		log.Printf("  → network: http://%s%s", lanIP, port)
	}

	// Graceful shutdown: clean up TAP devices on SIGTERM/SIGINT
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		log.Println("shutting down...")
		if shutdowner, ok := eng.(interface{ Shutdown() }); ok {
			shutdowner.Shutdown()
		}
		os.Exit(0)
	}()

	if err := http.ListenAndServe(cfg.Listen, srv); err != nil {
		log.Fatal(err)
	}
}

// recoverVMs restores Firecracker VMs from the SQLite store on startup.
func recoverVMs(st *store.Store, provider engine.VMStateProvider) {
	sandboxes, err := st.ListSandboxes()
	if err != nil {
		log.Printf("warning: recovery list: %v", err)
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
			// Has snapshot — can be resumed
			if _, err := os.Stat(fcState.SnapMemPath); err == nil {
				provider.RestoreVM(sb.EngineID, sb.Name, "stopped", state)
				recovered++
				log.Printf("recovered stopped sandbox %s (%s)", sb.Name, sb.ID)
			} else {
				st.UpdateSandboxStatus(sb.ID, "unknown")
				log.Printf("sandbox %s snapshot missing, marked unknown", sb.Name)
			}
		} else if sb.Status == "running" {
			// Was running — FC process is dead after server restart.
			if fcState.SnapMemPath != "" {
				st.UpdateSandboxStatus(sb.ID, "stopped")
				provider.RestoreVM(sb.EngineID, sb.Name, "stopped", state)
				recovered++
				log.Printf("sandbox %s was running, marked stopped (resumable)", sb.Name)
			} else {
				st.UpdateSandboxStatus(sb.ID, "unknown")
				log.Printf("sandbox %s was running with no snapshot, marked unknown", sb.Name)
			}
		}
	}

	if recovered > 0 {
		log.Printf("recovered %d sandbox(es) from store", recovered)
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
