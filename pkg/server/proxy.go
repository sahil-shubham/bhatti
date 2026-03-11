package server

import (
	"context"
	"io"
	"net"
	"sync"

	"github.com/sahilshubham/bhatti/pkg/engine"
)

// ForwardEntry represents an active port forward from host to sandbox.
type ForwardEntry struct {
	SandboxID     string `json:"sandbox_id"`
	EngineID      string `json:"-"`
	ContainerPort int    `json:"container_port"`
	HostPort      int    `json:"host_port"`
	listener      net.Listener
	cancel        context.CancelFunc
}

// ProxyManager manages TCP port forwards from the host to sandbox containers.
// Forwards are tunneled through Engine.Tunnel() — no direct network access
// to the sandbox is required. This works on Docker Desktop (where container
// IPs are unreachable) and will work identically with Firecracker via vsock.
type ProxyManager struct {
	mu        sync.Mutex
	engine    engine.Engine
	forwards  map[string]map[int]*ForwardEntry // sandboxID → containerPort → entry
	usedPorts map[int]string                   // hostPort → sandboxID
}

// NewProxyManager creates a new ProxyManager.
func NewProxyManager(eng engine.Engine) *ProxyManager {
	return &ProxyManager{
		engine:    eng,
		forwards:  make(map[string]map[int]*ForwardEntry),
		usedPorts: make(map[int]string),
	}
}

// Forward creates a TCP port forward from a random host port to containerPort
// inside the sandbox, tunneled through Engine.Tunnel(). Returns the existing
// forward if already active.
func (pm *ProxyManager) Forward(sandboxID, engineID string, containerPort int) (*ForwardEntry, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if fwd, exists := pm.forwards[sandboxID][containerPort]; exists {
		return fwd, nil
	}

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}

	hostPort := ln.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithCancel(context.Background())

	entry := &ForwardEntry{
		SandboxID:     sandboxID,
		EngineID:      engineID,
		ContainerPort: containerPort,
		HostPort:      hostPort,
		listener:      ln,
		cancel:        cancel,
	}

	go pm.accept(ctx, ln, engineID, containerPort)

	if pm.forwards[sandboxID] == nil {
		pm.forwards[sandboxID] = make(map[int]*ForwardEntry)
	}
	pm.forwards[sandboxID][containerPort] = entry
	pm.usedPorts[hostPort] = sandboxID
	return entry, nil
}

// accept handles incoming connections on the host-side listener.
// Each connection spawns a new Engine.Tunnel() into the sandbox.
func (pm *ProxyManager) accept(ctx context.Context, ln net.Listener, engineID string, port int) {
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
			tunnel, err := pm.engine.Tunnel(context.Background(), engineID, port)
			if err != nil {
				return
			}
			defer tunnel.Close()
			done := make(chan struct{})
			go func() {
				io.Copy(tunnel, c)
				close(done)
			}()
			io.Copy(c, tunnel)
			<-done
		}(conn)
	}
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
