package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

// StartMetricsSnapshots starts a background goroutine that records
// a metrics snapshot every 60 seconds. Counter fields are deltas since
// the previous snapshot. Gauge fields are point-in-time values.
func (s *Server) StartMetricsSnapshots() {
	ctx, cancel := context.WithCancel(context.Background())
	s.stopMetrics = cancel

	go func() {
		// Initialize previous counter values to current (which are 0 on
		// fresh start). First snapshot will have correct deltas.
		prevAPI := s.requestTotal.Load()
		prevErrors := s.requestErrors.Load()
		prevAuth := s.authFailures.Load()
		prevRateLimited := s.rateLimited.Load()

		var prevProxy, prevProxyErr, prevProxyWakes, prevProxyRL int64
		if s.publicProxy != nil {
			prevProxy = s.publicProxy.requestsTotal.Load()
			prevProxyErr = s.publicProxy.requestsError.Load()
			prevProxyWakes = s.publicProxy.coldWakes.Load()
			prevProxyRL = s.publicProxy.rateLimited.Load()
		}

		var prevDropped int64
		if s.events != nil {
			prevDropped = s.events.Dropped.Load()
		}

		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				snap := s.collectSnapshot(
					&prevAPI, &prevErrors, &prevAuth, &prevRateLimited,
					&prevProxy, &prevProxyErr, &prevProxyWakes, &prevProxyRL,
					&prevDropped,
				)
				if err := s.store.InsertMetricsSnapshot(snap); err != nil {
					slog.Error("metrics snapshot failed", "error", err)
				}
			}
		}
	}()
}

func (s *Server) collectSnapshot(
	prevAPI, prevErrors, prevAuth, prevRateLimited *int64,
	prevProxy, prevProxyErr, prevProxyWakes, prevProxyRL *int64,
	prevDropped *int64,
) store.MetricsSnapshot {
	// Compute counter deltas
	curAPI := s.requestTotal.Load()
	curErrors := s.requestErrors.Load()
	curAuth := s.authFailures.Load()
	curRateLimited := s.rateLimited.Load()

	snap := store.MetricsSnapshot{
		APIRequests:     curAPI - *prevAPI,
		APIErrors:       curErrors - *prevErrors,
		APIAuthFailures: curAuth - *prevAuth,
		APIRateLimited:  curRateLimited - *prevRateLimited,
	}
	*prevAPI = curAPI
	*prevErrors = curErrors
	*prevAuth = curAuth
	*prevRateLimited = curRateLimited

	// Proxy counters
	if s.publicProxy != nil {
		curProxy := s.publicProxy.requestsTotal.Load()
		curProxyErr := s.publicProxy.requestsError.Load()
		curProxyWakes := s.publicProxy.coldWakes.Load()
		curProxyRL := s.publicProxy.rateLimited.Load()

		snap.ProxyRequests = curProxy - *prevProxy
		snap.ProxyErrors = curProxyErr - *prevProxyErr
		snap.ProxyColdWakes = curProxyWakes - *prevProxyWakes
		snap.ProxyRateLimited = curProxyRL - *prevProxyRL

		*prevProxy = curProxy
		*prevProxyErr = curProxyErr
		*prevProxyWakes = curProxyWakes
		*prevProxyRL = curProxyRL

		snap.WebsocketsActive = int(s.publicProxy.webSocketActive.Load())
	}

	// EventRecorder dropped counter
	if s.events != nil {
		curDropped := s.events.Dropped.Load()
		snap.EventsDropped = curDropped - *prevDropped
		*prevDropped = curDropped
	}

	// Gauge: sandbox counts by thermal state
	sandboxes, _ := s.store.ListAllSandboxes()
	snap.SandboxesTotal = len(sandboxes)
	if te, ok := s.engine.(ThermalEngine); ok {
		for _, sb := range sandboxes {
			if sb.Status != "running" {
				snap.SandboxesCold++
				continue
			}
			switch te.ThermalState(sb.EngineID) {
			case "hot":
				snap.SandboxesHot++
			case "warm":
				snap.SandboxesWarm++
			default:
				snap.SandboxesCold++
			}
		}
	} else {
		for _, sb := range sandboxes {
			if sb.Status == "running" {
				snap.SandboxesHot++
			} else {
				snap.SandboxesCold++
			}
		}
	}

	// Gauge: user counts
	users, _ := s.store.ListUsers()
	snap.UsersTotal = len(users)
	userHasSandbox := make(map[string]bool)
	for _, sb := range sandboxes {
		userHasSandbox[sb.CreatedBy] = true
	}
	snap.UsersActive = len(userHasSandbox)

	// Gauge: host stats (Linux only, graceful on others)
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		fmt.Sscanf(string(data), "%f", &snap.HostLoad1m)
	}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				var kb int64
				fmt.Sscanf(line, "MemTotal: %d kB", &kb)
				snap.HostMemTotalMB = int(kb / 1024)
			}
			if strings.HasPrefix(line, "MemAvailable:") {
				var kb int64
				fmt.Sscanf(line, "MemAvailable: %d kB", &kb)
				snap.HostMemAvailMB = int(kb / 1024)
			}
		}
	}

	return snap
}

// StartRetention starts a background goroutine that purges old events
// and metrics snapshots hourly.
func (s *Server) StartRetention() {
	ctx, cancel := context.WithCancel(context.Background())
	s.stopRetention = cancel

	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n, err := s.store.PurgeOldEvents(90 * 24 * time.Hour); err != nil {
					slog.Warn("event retention failed", "error", err)
				} else if n > 0 {
					slog.Info("purged old events", "count", n)
				}
				if n, err := s.store.PurgeOldMetricsSnapshots(30 * 24 * time.Hour); err != nil {
					slog.Warn("metrics retention failed", "error", err)
				} else if n > 0 {
					slog.Info("purged old metrics snapshots", "count", n)
				}
			}
		}
	}()
}
