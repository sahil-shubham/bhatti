package krucible

import "time"

// procArgs can briefly fail while a process is execing. Do not turn one
// unreadable argv into a false death notification for a still-live guest.
func processIdentityGone(matches func() bool, stop <-chan struct{}) bool {
	for range 3 {
		if matches() {
			return false
		}
		select {
		case <-stop:
			return false
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	return !matches()
}

func pollProcessExit(matches func() bool, stop <-chan struct{}) bool {
	if processIdentityGone(matches, stop) {
		return true
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return false
		case <-ticker.C:
			if processIdentityGone(matches, stop) {
				return true
			}
		}
	}
}
