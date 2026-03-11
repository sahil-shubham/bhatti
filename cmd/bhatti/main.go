package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/sahilshubham/bhatti/pkg"
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
	eng, err := docker.New()
	if err != nil {
		log.Fatal(err)
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

	if err := http.ListenAndServe(cfg.Listen, srv); err != nil {
		log.Fatal(err)
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
