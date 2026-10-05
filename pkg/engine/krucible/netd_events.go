package krucible

// netdEvent is queued during Engine.New recovery, before the API server's
// event recorder exists. The owner and sandbox IDs are the durable identities.
type netdEvent struct {
	userID, sandboxID, reason string
}

// SetNetdEventRecorder attaches the server's recorder and flushes events from
// recovery. It is per-engine, not a package-wide callback shared by instances.
func (e *Engine) SetNetdEventRecorder(record func(userID, sandboxID, reason string)) {
	e.netdEventMu.Lock()
	e.onNetdEvent = record
	if record == nil {
		e.netdEventMu.Unlock()
		return
	}
	pending := e.pendingNetdEvents
	e.pendingNetdEvents = nil
	e.netdEventMu.Unlock()
	for _, event := range pending {
		record(event.userID, event.sandboxID, event.reason)
	}
}

func (e *Engine) emitNetdEvent(userID, sandboxID, reason string) {
	e.netdEventMu.Lock()
	record := e.onNetdEvent
	if record == nil {
		e.pendingNetdEvents = append(e.pendingNetdEvents, netdEvent{userID, sandboxID, reason})
	}
	e.netdEventMu.Unlock()
	if record != nil {
		record(userID, sandboxID, reason)
	}
}
