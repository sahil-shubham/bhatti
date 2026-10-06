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

// StartKeepHotRecovery wakes previously stopped keep_hot guests after startup.
// Unlike a detached daemon goroutine, this wake is cancelled and joined by Close.
func (s *Server) StartKeepHotRecovery() {
	s.startBackground("keep-hot recovery", func() {
		sandboxes, err := s.store.ListAllSandboxes()
		if err != nil {
			slog.Warn("auto-wake: list sandboxes", "error", err)
			return
		}
		for _, sb := range sandboxes {
			if s.ctx.Err() != nil {
				return
			}
			if !sb.KeepHot || sb.Status == "destroyed" || s.lifecycle.isRestarting(sb.EngineID) {
				continue
			}
			ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
			err := s.EnsureHot(ctx, sb.EngineID)
			cancel()
			if err != nil {
				if s.ctx.Err() == nil {
					slog.Error("auto-wake failed", "sandbox", sb.Name, "id", sb.ID, "error", err)
				}
			} else {
				slog.Info("auto-wake: sandbox started", "sandbox", sb.Name, "id", sb.ID)
			}
		}
	})
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
		if err := s.store.StopSandbox(sb.ID); err != nil {
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

func (l *serverLifecycle) finishRestart(engineID string) bool {
	l.restartMu.Lock()
	defer l.restartMu.Unlock()
	state := l.restarts[engineID]
	state.active = false
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

func (s *Server) restartFailed(sb *store.Sandbox, reason string) {
	slog.Error("sandbox.restart_failed", "sandbox_id", sb.ID, "reason", reason)
	s.RecordEvent(store.Event{
		Type: "sandbox.restart_failed", UserID: sb.CreatedBy, SandboxID: sb.ID,
		Meta: map[string]any{"name": sb.Name, "reason": reason},
	})
}
