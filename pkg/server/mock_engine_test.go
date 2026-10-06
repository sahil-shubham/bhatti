package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// mockEngine implements engine.Engine and ThermalEngine for server tests.
// Minimal — just enough for HTTP layer testing. Real engine behavior
// is tested by pkg/engine/firecracker/ on agni-01.
type mockEngine struct {
	mu        sync.Mutex
	sandboxes map[string]*engine.SandboxInfo
	thermal   map[string]string // engineID → thermal state
	nextID    atomic.Int64

	// Configurable per-test
	ExecResult      engine.ExecResult
	CreateErr       error
	ExecErr         error
	StopErr         error
	ActivityResult  *proto.ActivityInfo
	ActivityErr     error
	GuestFeatureErr error
	StartErr        error
	StartCalls      int
	StartHook       func(string) // runs outside m.mu after a successful start
	onLifecycle     func(engine.LifecycleEvent)
	pendingEvents   []engine.LifecycleEvent

	// LastCreateSpec is the spec from the most recent successful Create call,
	// for tests that want to verify what got passed downstream.
	LastCreateSpec engine.SandboxSpec

	// ExecStarted/ExecRelease, when set, make Exec signal that it started and
	// block until released (a command still running).
	ExecStarted chan struct{}
	ExecRelease chan struct{}
	// Inits answers HasInit: engine ID → has an --init command. Absent = unknown.
	Inits map[string]bool
	// TunnelRefusals makes the next N Tunnel calls fail as "nothing listening".
	TunnelRefusals int
	// TunnelPeer receives the guest end of each successful tunnel. Tests set
	// a buffered channel when they need to exchange bytes with the client.
	TunnelPeer chan net.Conn

	// Stops and Shutdowns count Stop and Shutdown calls.
	Stops, Shutdowns int
}

func newMockEngine() *mockEngine {
	return &mockEngine{
		sandboxes: make(map[string]*engine.SandboxInfo),
		thermal:   make(map[string]string),
		ExecResult: engine.ExecResult{
			ExitCode: 0,
			Stdout:   "mock output\n",
		},
	}
}

func (m *mockEngine) Create(_ context.Context, spec engine.SandboxSpec) (engine.SandboxInfo, error) {
	if m.CreateErr != nil {
		return engine.SandboxInfo{}, m.CreateErr
	}
	id := fmt.Sprintf("mock-%d", m.nextID.Add(1))
	name := spec.Name
	if name == "" {
		name = id
	}
	info := engine.SandboxInfo{
		ID:       id,
		Name:     name,
		Status:   "running",
		IP:       "10.0.1.2",
		EngineID: id,
	}
	m.mu.Lock()
	m.sandboxes[id] = &info
	m.LastCreateSpec = spec
	m.mu.Unlock()
	return info, nil
}

func (m *mockEngine) Destroy(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sandboxes[id]; !ok {
		return fmt.Errorf("sandbox %q not found", id)
	}
	delete(m.sandboxes, id)
	return nil
}

func (m *mockEngine) Stop(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Stops++
	if m.StopErr != nil {
		return m.StopErr
	}
	sb, ok := m.sandboxes[id]
	if !ok {
		return fmt.Errorf("sandbox %q not found", id)
	}
	sb.Status = "stopped"
	m.thermal[id] = "cold"
	return nil
}

// Shutdown is the engine's half of the daemon's shutdown.
func (m *mockEngine) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Shutdowns++
}

func (m *mockEngine) Start(_ context.Context, id string) error {
	m.mu.Lock()
	m.StartCalls++
	if m.StartErr != nil {
		err := m.StartErr
		m.mu.Unlock()
		return err
	}
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("sandbox %q not found", id)
	}
	sb.Status = "running"
	m.thermal[id] = "hot"
	hook := m.StartHook
	m.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	return nil
}

func (m *mockEngine) SetLifecycleHandler(handler func(engine.LifecycleEvent)) {
	m.mu.Lock()
	m.onLifecycle = handler
	pending := m.pendingEvents
	m.pendingEvents = nil
	m.mu.Unlock()
	if handler != nil {
		for _, e := range pending {
			handler(e)
		}
	}
}

// emitLifecycle simulates the engine having already persisted an exit before
// delivering its notification, including events from recovery before wiring.
func (m *mockEngine) emitLifecycle(e engine.LifecycleEvent) {
	m.mu.Lock()
	if e.Kind == engine.VMExited {
		if sb := m.sandboxes[e.EngineID]; sb != nil {
			sb.Status = "stopped"
			m.thermal[e.EngineID] = "cold"
		}
	}
	handler := m.onLifecycle
	if handler == nil {
		m.pendingEvents = append(m.pendingEvents, e)
	}
	m.mu.Unlock()
	if handler != nil {
		handler(e)
	}
}

func (m *mockEngine) Status(_ context.Context, id string) (engine.SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[id]
	if !ok {
		return engine.SandboxInfo{}, fmt.Errorf("sandbox %q not found", id)
	}
	return *sb, nil
}

func (m *mockEngine) List(_ context.Context) ([]engine.SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []engine.SandboxInfo
	for _, sb := range m.sandboxes {
		out = append(out, *sb)
	}
	return out, nil
}

func (m *mockEngine) Exec(_ context.Context, id string, cmd []string) (engine.ExecResult, error) {
	m.mu.Lock()
	_, ok := m.sandboxes[id]
	m.mu.Unlock()
	if !ok {
		return engine.ExecResult{}, fmt.Errorf("sandbox %q not found", id)
	}
	if m.ExecErr != nil {
		return engine.ExecResult{}, m.ExecErr
	}
	if m.ExecStarted != nil {
		m.ExecStarted <- struct{}{}
		<-m.ExecRelease
	}
	return m.ExecResult, nil
}

func (m *mockEngine) Shell(_ context.Context, id string) (engine.TerminalConn, error) {
	m.mu.Lock()
	_, ok := m.sandboxes[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("sandbox %q not found", id)
	}
	// Return a pipe-based terminal. The server writes to one end,
	// the test reads from the other.
	server, client := net.Pipe()
	// Send a welcome message so the terminal isn't empty
	go func() {
		server.Write([]byte("$ "))
	}()
	return &mockTermConn{conn: client, server: server}, nil
}

func (m *mockEngine) ListeningPorts(_ context.Context, id string) ([]int, error) {
	m.mu.Lock()
	_, ok := m.sandboxes[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("sandbox %q not found", id)
	}
	return []int{}, nil
}

func (m *mockEngine) Tunnel(_ context.Context, id string, port int) (io.ReadWriteCloser, error) {
	m.mu.Lock()
	_, ok := m.sandboxes[id]
	refuse := m.TunnelRefusals > 0
	if refuse {
		m.TunnelRefusals--
	}
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("sandbox %q not found", id)
	}
	if refuse {
		return nil, fmt.Errorf("forward to port %d: %w: connection refused", port, agent.ErrPortRefused)
	}
	guest, client := net.Pipe()
	if peer := m.TunnelPeer; peer != nil {
		peer <- guest
	} else {
		guest.Close()
	}
	return client, nil
}

// --- ThermalEngine interface ---

func (m *mockEngine) ThermalState(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.thermal[id]
}

func (m *mockEngine) Pause(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sandboxes[id]; !ok {
		return fmt.Errorf("sandbox %q not found", id)
	}
	m.thermal[id] = "warm"
	return nil
}

func (m *mockEngine) EnsureHot(ctx context.Context, id string) error {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("sandbox %q not found", id)
	}
	stopped := sb.Status == "stopped"
	if !stopped {
		m.thermal[id] = "hot"
	}
	m.mu.Unlock()
	if stopped {
		return m.Start(ctx, id)
	}
	return nil
}

func (m *mockEngine) Activity(_ context.Context, id string) (*proto.ActivityInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ActivityErr != nil {
		return nil, m.ActivityErr
	}
	if m.ActivityResult != nil {
		return m.ActivityResult, nil
	}
	// Default: idle for a long time, no sessions
	return &proto.ActivityInfo{
		LastActivityUnix: 0,
		AttachedSessions: 0,
	}, nil
}

func (m *mockEngine) RequireGuestAgentFeature(_ context.Context, _ string, _ proto.AgentFeature) error {
	return m.GuestFeatureErr
}

// mockTermConn wraps net.Pipe as engine.TerminalConn.
type mockTermConn struct {
	conn   net.Conn
	server net.Conn
}

func (t *mockTermConn) Read(p []byte) (int, error)  { return t.conn.Read(p) }
func (t *mockTermConn) Write(p []byte) (int, error) { return t.conn.Write(p) }
func (t *mockTermConn) Resize(rows, cols int) error { return nil }
func (t *mockTermConn) Close() error {
	t.server.Close()
	return t.conn.Close()
}

func (m *mockEngine) HasInit(id string) (bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	has, ok := m.Inits[id]
	return has, ok
}
