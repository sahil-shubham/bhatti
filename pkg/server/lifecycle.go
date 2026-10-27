package server

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

const restartWindow = 10 * time.Minute

var restartBackoff = [...]time.Duration{0, 30 * time.Second, 2 * time.Minute}

type restartState struct {
	attempts []time.Time // recent crashes and failed boots, including the current attempt
	active   bool
	pending  bool // another exit arrived while this VM's restart was in flight
	aborted  bool // the owner stopped the sandbox while a restart was waiting
}

type serverLifecycle struct {
	mu      sync.Mutex
	queue   []engine.LifecycleEvent
	head    int
	wake    chan struct{}
	started bool

	restartMu sync.Mutex
	restarts  map[string]*restartState
	now       func() time.Time
	wait      func(context.Context, time.Duration) bool
}

func newServerLifecycle() *serverLifecycle {
	return &serverLifecycle{
		wake:     make(chan struct{}, 1),
		restarts: make(map[string]*restartState),
		now:      time.Now,
		wait: func(ctx context.Context, delay time.Duration) bool {
			if delay == 0 {
				return ctx.Err() == nil
			}
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return false
			case <-timer.C:
				return ctx.Err() == nil
			}
		},
	}
}

// StartLifecycleEvents connects engine-originated exits and network loss after
// startup store recovery. The callback only enqueues: engine waiters never do
// SQLite or restart work. Events queued by the engine before registration are
// delivered through the same path.
func (s *Server) StartLifecycleEvents() {
	reporter, ok := s.engine.(engine.LifecycleReporter)
	if !ok {
		return
	}
	s.lifecycle.mu.Lock()
	if s.lifecycle.started {
		s.lifecycle.mu.Unlock()
		return
	}
	s.lifecycle.started = true
	s.lifecycle.mu.Unlock()
	if s.startBackground("engine lifecycle", s.consumeLifecycle) {
		reporter.SetLifecycleHandler(s.enqueueLifecycle)
	}
}

// StartKeepHotRecovery restarts the keep_hot sandboxes marked restart_pending:
// stored stopped because they went down on their own (a crash whose restart
// had not run yet, or the daemon or host going down), never because their
// owner stopped them — an owner's stop, a destroy, or keep_hot turning off
// all clear the marker.
//
// Each goes through the same scheduler as a crash restart: try now, then after
// 30s and 2m, and give up (sandbox.restart_failed) after three failures in ten
// minutes. A transient failure right after a host reboot (gateway not up yet,
// disk busy) therefore does not strand the guest. The marker stays set while
// it backs off, so a daemon that exits mid-backoff leaves the retry to the next
// start. The restart workers are cancelled and joined by Close.
func (s *Server) StartKeepHotRecovery() {
	pending, err := s.store.ListRestartPending()
	if err != nil {
		slog.Warn("keep-hot recovery: list pending restarts", "error", err)
		return
	}
	for i := range pending {
		s.scheduleRestart(&pending[i])
	}
}

func (s *Server) enqueueLifecycle(e engine.LifecycleEvent) {
	l := s.lifecycle
	l.mu.Lock()
	if s.ctx.Err() == nil {
		l.queue = append(l.queue, e)
		select {
		case l.wake <- struct{}{}:
		default:
		}
	}
	l.mu.Unlock()
}

func (s *Server) consumeLifecycle() {
	l := s.lifecycle
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-l.wake:
		}
		for {
			if s.ctx.Err() != nil {
				return
			}
			l.mu.Lock()
			if l.head == len(l.queue) {
				l.mu.Unlock()
				break
			}
			e := l.queue[l.head]
			l.queue[l.head] = engine.LifecycleEvent{}
			l.head++
			if l.head == len(l.queue) {
				l.queue = l.queue[:0]
				l.head = 0
			}
			l.mu.Unlock()
			s.handleLifecycle(e)
		}
	}
}

func (s *Server) handleLifecycle(e engine.LifecycleEvent) {
	var sb *store.Sandbox
	var err error
	if e.EngineID != "" {
		sb, err = s.store.GetSandboxByEngineID(e.EngineID)
	}
	if (e.EngineID == "" || err != nil) && e.SandboxID != "" {
		sb, err = s.store.GetSandboxByID(e.SandboxID)
	}
	if err != nil {
		slog.Warn("engine lifecycle: sandbox lookup failed", "engine_id", e.EngineID, "sandbox_id", e.SandboxID, "error", err)
	}
	if sb != nil && sb.Status == "destroyed" {
		return
	}

	switch e.Kind {
	case engine.VMExited:
		if sb == nil {
			return
		}
		// Serialize exit persistence with a concurrent successful Start. An
		// exit that wins the race must not be overwritten by the Start worker.
		gate := s.lockTransition(sb.EngineID)
		defer s.unlockTransition(sb.EngineID, gate)
		if err := s.store.StopSandboxUnexpected(sb.ID); err != nil {
			slog.Error("sandbox exit: persist stopped state", "sandbox_id", sb.ID, "error", err)
			return
		}
		s.lastActivity.Delete(sb.EngineID)
		s.snapshotFailures.Delete(sb.EngineID)
		s.thermalFails.Delete(sb.EngineID)
		s.RecordEvent(store.Event{
			Type: "sandbox.exited", UserID: sb.CreatedBy, SandboxID: sb.ID,
			Meta: map[string]any{"name": sb.Name, "exit_code": e.ExitCode, "signal": e.Signal, "reason": e.Reason},
		})
		if sb.KeepHot {
			s.scheduleRestart(sb)
		}
	case engine.NetworkLost:
		userID, sandboxID := e.UserID, e.SandboxID
		if sb != nil {
			userID, sandboxID = sb.CreatedBy, sb.ID
		}
		s.RecordEvent(store.Event{
			Type: "sandbox.network_lost", UserID: userID, SandboxID: sandboxID,
			Meta: map[string]any{"reason": e.Reason},
		})
	default:
		slog.Warn("unknown engine lifecycle event", "kind", e.Kind)
	}
}

// reserveRestart counts both unexpected exits and unsuccessful cold starts.
// Three attempts in the rolling window get delays of 0, 30s, and 2m; a
// fourth failure cannot start another VM until the window expires.
func (l *serverLifecycle) reserveRestart(engineID string, retry bool) (time.Duration, bool, bool) {
	l.restartMu.Lock()
	defer l.restartMu.Unlock()
	state := l.restarts[engineID]
	if state == nil {
		state = &restartState{}
		l.restarts[engineID] = state
	}
	if state.active && !retry {
		state.pending = true
		return 0, false, false
	}
	now := l.now()
	cutoff := now.Add(-restartWindow)
	recent := state.attempts[:0]
	for _, when := range state.attempts {
		if when.After(cutoff) {
			recent = append(recent, when)
		}
	}
	state.attempts = recent
	if len(recent) >= len(restartBackoff) {
		return 0, false, true
	}
	delay := restartBackoff[len(recent)]
	state.attempts = append(state.attempts, now)
	state.active = true
	return delay, true, false
}

func (l *serverLifecycle) isRestarting(engineID string) bool {
	l.restartMu.Lock()
	defer l.restartMu.Unlock()
	state := l.restarts[engineID]
	return state != nil && state.active
}

// abortRestart cancels a waiting or pending keep-hot restart. A crashed
// keep_hot sandbox is stored stopped while it backs off, so an explicit stop
// leaves the store unchanged; without this the restart would boot the VM the
// owner just stopped.
func (l *serverLifecycle) abortRestart(engineID string) {
	l.restartMu.Lock()
	defer l.restartMu.Unlock()
	if state := l.restarts[engineID]; state != nil && state.active {
		state.aborted = true
		state.pending = false
	}
}

func (l *serverLifecycle) restartAborted(engineID string) bool {
	l.restartMu.Lock()
	defer l.restartMu.Unlock()
	state := l.restarts[engineID]
	return state != nil && state.aborted
}

func (l *serverLifecycle) finishRestart(engineID string) bool {
	l.restartMu.Lock()
	defer l.restartMu.Unlock()
	state := l.restarts[engineID]
	state.active = false
	state.aborted = false
	pending := state.pending
	state.pending = false
	return pending
}

func (s *Server) completeRestart(sandboxID, engineID string) {
	if !s.lifecycle.finishRestart(engineID) || s.ctx.Err() != nil {
		return
	}
	sb, err := s.store.GetSandboxByID(sandboxID)
	if err == nil && sb.KeepHot && sb.Status == "stopped" {
		s.scheduleRestart(sb)
	}
}

func (s *Server) scheduleRestart(sb *store.Sandbox) {
	delay, scheduled, exhausted := s.lifecycle.reserveRestart(sb.EngineID, false)
	if exhausted {
		s.restartFailed(sb, "three exits or failed boots within 10 minutes")
		return
	}
	if !scheduled {
		return
	}
	if !s.startBackground("keep-hot restart", func() { s.restartKeepHot(sb.ID, sb.EngineID, delay) }) {
		s.lifecycle.finishRestart(sb.EngineID)
	}
}

func (s *Server) restartKeepHot(sandboxID, engineID string, delay time.Duration) {
	active := true
	defer func() {
		if active {
			s.completeRestart(sandboxID, engineID)
		}
	}()
	for {
		if !s.lifecycle.wait(s.ctx, delay) {
			return
		}
		if s.lifecycle.restartAborted(engineID) {
			return // the owner stopped it while the restart was backing off
		}
		sb, err := s.store.GetSandboxByID(sandboxID)
		if err != nil || !sb.KeepHot || sb.Status != "stopped" {
			return // destroyed, disabled, or already woken by a request
		}
		if err := s.checkStoredMounts(engineID); err != nil {
			s.restartFailed(sb, fmt.Sprintf("mount policy: %v", err))
			return
		}
		ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
		err = s.engine.Start(ctx, engineID)
		cancel()
		if s.ctx.Err() != nil {
			return
		}
		if err == nil {
			gate := s.lockTransition(engineID)
			// Start can return just as its helper exits. Inspect the engine
			// while holding this VM's gate before persisting a running state.
			info, statusErr := s.engine.Status(s.ctx, engineID)
			if statusErr == nil && info.Status != "running" {
				s.unlockTransition(engineID, gate)
				return // the exit event will schedule the next attempt
			}
			if statusErr == nil {
				statusErr = s.store.UpdateSandboxStatus(sb.ID, "running")
			}
			if statusErr == nil {
				s.touchActivity(engineID)
				s.lifecycle.finishRestart(engineID)
				active = false
			}
			s.unlockTransition(engineID, gate)
			if statusErr != nil {
				err = fmt.Errorf("confirm restarted VM: %w", statusErr)
			} else {
				slog.Info("sandbox.restarted", "sandbox_id", sb.ID, "name", sb.Name)
				s.RecordEvent(store.Event{
					Type: "sandbox.restarted", UserID: sb.CreatedBy, SandboxID: sb.ID,
					Meta: map[string]any{"name": sb.Name},
				})
				return
			}
		}
		slog.Warn("keep-hot restart failed", "sandbox_id", sb.ID, "error", err)
		var exhausted bool
		delay, _, exhausted = s.lifecycle.reserveRestart(engineID, true)
		if exhausted {
			s.restartFailed(sb, err.Error())
			return
		}
	}
}

// restartFailed gives up on a keep_hot sandbox: clearing the marker keeps a
// VM that cannot boot from being retried on every daemon restart.
func (s *Server) restartFailed(sb *store.Sandbox, reason string) {
	slog.Error("sandbox.restart_failed", "sandbox_id", sb.ID, "reason", reason)
	if err := s.store.ClearRestartPending(sb.ID); err != nil {
		slog.Warn("sandbox.restart_failed: clear restart marker", "sandbox_id", sb.ID, "error", err)
	}
	s.RecordEvent(store.Event{
		Type: "sandbox.restart_failed", UserID: sb.CreatedBy, SandboxID: sb.ID,
		Meta: map[string]any{"name": sb.Name, "reason": reason},
	})
}
