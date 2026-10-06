package server

import (
	"log/slog"
	"runtime/debug"
)

// goSafe keeps a panic in background work from taking down the daemon.
func goSafe(name string, fn func()) {
	go func() {
		defer func() {
			if p := recover(); p != nil {
				slog.Error("server goroutine panic", "name", name, "panic", p, "stack", string(debug.Stack()))
			}
		}()
		fn()
	}()
}
