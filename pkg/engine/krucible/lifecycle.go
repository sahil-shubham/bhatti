package krucible

import "github.com/sahil-shubham/bhatti/pkg/engine"

// SetLifecycleHandler drains recovery events before returning. Event delivery
// outside the lock lets a handler call back into the engine.
func (e *Engine) SetLifecycleHandler(handle func(engine.LifecycleEvent)) {
	e.eventMu.Lock()
	e.onLifecycleEvent = handle
	if handle == nil {
		e.eventMu.Unlock()
		return
	}
	pending := e.pendingEvents
	e.pendingEvents = nil
	e.eventMu.Unlock()
	for _, event := range pending {
		handle(event)
	}
}

func (e *Engine) emitLifecycle(event engine.LifecycleEvent) {
	e.eventMu.Lock()
	handle := e.onLifecycleEvent
	if handle == nil {
		e.pendingEvents = append(e.pendingEvents, event)
	}
	e.eventMu.Unlock()
	if handle != nil {
		handle(event)
	}
}

// A netd's Unixstream connections belong to the old process: the next
// ensureNetd serves new guests only, never silently claims to repair old ones.
func (e *Engine) emitNetworkLost(owner, reason string) {
	e.mu.RLock()
	vms := make([]*VM, 0)
	for _, vm := range e.vms {
		if vm.netdKey == owner {
			vms = append(vms, vm)
		}
	}
	e.mu.RUnlock()
	reason += "; running guests cannot reattach their Unixstream NIC until restarted"
	for _, vm := range vms {
		e.emitLifecycle(engine.LifecycleEvent{Kind: engine.NetworkLost, EngineID: vm.ID, SandboxID: vm.sandboxRef,
			UserID: vm.UserID, Reason: reason})
	}
}
