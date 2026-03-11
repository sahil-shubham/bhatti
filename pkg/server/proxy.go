package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// ForwardEntry represents an active port forward from host to sandbox.
type ForwardEntry struct {
	SandboxID     string `json:"sandbox_id"`
	SandboxIP     string `json:"sandbox_ip"`
	ContainerPort int    `json:"container_port"`
	HostPort      int    `json:"host_port"`
	listener      net.Listener
	cancel        context.CancelFunc
}

// ProxyManager manages TCP port forwards from the host to sandbox containers.
type ProxyManager struct {
	mu        sync.Mutex
	forwards  map[string]map[int]*ForwardEntry // sandboxID → containerPort → entry
	usedPorts map[int]string                   // hostPort → sandboxID
}

// NewProxyManager creates a new ProxyManager.
func NewProxyManager() *ProxyManager {
	return &ProxyManager{
		forwards:  make(map[string]map[int]*ForwardEntry),
		usedPorts: make(map[int]string),
	}
}

// Forward creates a TCP port forward from a random host port to containerPort
// on the given sandbox IP. Returns the existing forward if already active.
func (pm *ProxyManager) Forward(sandboxID, sandboxIP string, containerPort int) (*ForwardEntry, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Already forwarding this port for this sandbox?
	if fwd, exists := pm.forwards[sandboxID][containerPort]; exists {
		return fwd, nil
	}

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	hostPort := ln.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithCancel(context.Background())

	entry := &ForwardEntry{
		SandboxID:     sandboxID,
		SandboxIP:     sandboxIP,
		ContainerPort: containerPort,
		HostPort:      hostPort,
		listener:      ln,
		cancel:        cancel,
	}

	go pm.relay(ctx, ln, sandboxIP, containerPort)

	if pm.forwards[sandboxID] == nil {
		pm.forwards[sandboxID] = make(map[int]*ForwardEntry)
	}
	pm.forwards[sandboxID][containerPort] = entry
	pm.usedPorts[hostPort] = sandboxID
	return entry, nil
}

// StopForward stops a single port forward for a sandbox.
func (pm *ProxyManager) StopForward(sandboxID string, containerPort int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if fwds, ok := pm.forwards[sandboxID]; ok {
		if entry, ok := fwds[containerPort]; ok {
			entry.cancel()
			entry.listener.Close()
			delete(pm.usedPorts, entry.HostPort)
			delete(fwds, containerPort)
			if len(fwds) == 0 {
				delete(pm.forwards, sandboxID)
			}
		}
	}
}

// StopAll stops all port forwards for a sandbox.
func (pm *ProxyManager) StopAll(sandboxID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for _, entry := range pm.forwards[sandboxID] {
		entry.cancel()
		entry.listener.Close()
		delete(pm.usedPorts, entry.HostPort)
	}
	delete(pm.forwards, sandboxID)
}

// ActiveForwards returns all active forwards for a sandbox.
func (pm *ProxyManager) ActiveForwards(sandboxID string) []ForwardEntry {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	var out []ForwardEntry
	for _, entry := range pm.forwards[sandboxID] {
		out = append(out, *entry)
	}
	return out
}

// AllForwards returns all active forwards across all sandboxes.
func (pm *ProxyManager) AllForwards() []ForwardEntry {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	var out []ForwardEntry
	for _, fwds := range pm.forwards {
		for _, entry := range fwds {
			out = append(out, *entry)
		}
	}
	return out
}

// PortOwner returns the sandbox ID that owns a given host port, if any.
func (pm *ProxyManager) PortOwner(hostPort int) (string, bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	id, ok := pm.usedPorts[hostPort]
	return id, ok
}

func (pm *ProxyManager) relay(ctx context.Context, ln net.Listener, ip string, port int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}
		go func(c net.Conn) {
			defer c.Close()
			remote, err := net.DialTimeout("tcp",
				fmt.Sprintf("%s:%d", ip, port), 3*time.Second)
			if err != nil {
				return
			}
			defer remote.Close()
			done := make(chan struct{})
			go func() {
				io.Copy(remote, c)
				close(done)
			}()
			io.Copy(c, remote)
			<-done
		}(conn)
	}
}
