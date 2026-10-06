package krucible

import (
	"fmt"
	"os/exec"
)

// watchNetd owns the cmd.Wait result. It closes reaped before taking inst.mu:
// releaseNetd and replaceNetdLocked may wait for it while holding that lock.
func (e *Engine) watchNetd(inst *netdInstance, cmd *exec.Cmd, done <-chan error, reaped chan struct{}) {
	stop := e.monitorChannel()
	go func() {
		var err error
		select {
		case err = <-done:
		case <-stop:
			<-done // detached netd stays this daemon's child until it exits
			close(reaped)
			return
		}
		close(reaped)
		select {
		case <-stop:
			return
		default:
		}
		inst.mu.Lock()
		if inst.cmd != cmd || inst.stopping {
			inst.mu.Unlock()
			return
		}
		e.netdExitedLocked(inst)
		inst.mu.Unlock()
		e.emitNetworkLost(inst.owner, fmt.Sprintf("bhatti-netd exited: %v", err))
	}()
}

// Called with inst.mu held. An adopted gateway can die while the daemon is
// probing the old socket; only one watcher is installed for each pid.
func (e *Engine) watchAdoptedNetdLocked(inst *netdInstance) {
	pid, sock := inst.pid, inst.sock
	if pid <= 0 || inst.watchedPID == pid {
		return
	}
	inst.watchedPID = pid
	stop := e.monitorChannel()
	go func() {
		if !waitProcessExit(pid, func() bool { return pidAlive(pid) && isNetd(pid, sock) }, stop) {
			return
		}
		select {
		case <-stop:
			return
		default:
		}
		inst.mu.Lock()
		if inst.pid != pid || inst.stopping {
			inst.mu.Unlock()
			return
		}
		e.netdExitedLocked(inst)
		inst.mu.Unlock()
		e.emitNetworkLost(inst.owner, "adopted bhatti-netd exited (exit status unavailable: not this daemon's child)")
	}()
}

func (e *Engine) netdExitedLocked(inst *netdInstance) {
	inst.cmd, inst.waitDone, inst.exitDone = nil, nil, nil
	inst.pid, inst.watchedPID = 0, 0
	writeNetdRecord(inst)
	if inst.brokerLn != nil {
		_ = inst.brokerLn.Close()
		inst.brokerLn = nil
	}
}
