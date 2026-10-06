package krucible

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// monitorChannel stops adopted-process monitors on daemon shutdown without
// stopping the processes being adopted. Owned children still have their one
// cmd.Wait goroutine, which may reap them after this daemon shuts down.
func (e *Engine) monitorChannel() <-chan struct{} {
	e.monitorMu.Lock()
	defer e.monitorMu.Unlock()
	if e.monitorStop == nil {
		e.monitorStop = make(chan struct{})
		if e.monitorStopped {
			close(e.monitorStop)
		}
	}
	return e.monitorStop
}

func (e *Engine) stopMonitors() {
	e.monitorMu.Lock()
	if !e.monitorStopped {
		e.monitorStopped = true
		if e.monitorStop != nil {
			close(e.monitorStop)
		}
	}
	e.monitorMu.Unlock()
}

func (e *Engine) watchVM(vm *VM, cmd *exec.Cmd, done <-chan error, reaped chan struct{}) {
	stop := e.monitorChannel()
	go func() {
		var err error
		select {
		case err = <-done:
		case <-stop:
			// Shutdown abandons the VM, not its child-reaping obligation.
			// Keep the only Cmd.Wait owner until this process eventually exits.
			<-done
			close(reaped)
			return
		}
		close(reaped) // the killer must not wait on launchMu, which it holds
		select {
		case <-stop:
			return
		default:
		}
		vm.launchMu.Lock()
		defer vm.launchMu.Unlock()
		vm.mu.Lock()
		if vm.cmd != cmd || vm.stopping || vm.Status != "running" {
			vm.mu.Unlock()
			return
		}
		vm.mu.Unlock()
		e.unexpectedVMExit(vm, exitEvent(vm, err))
	}()
}

func (e *Engine) watchAdoptedVM(vm *VM) {
	pid, spec := vm.HelperPID, vm.specPath()
	stop := e.monitorChannel()
	go func() {
		if !waitProcessExit(pid, func() bool { return pidAlive(pid) && isHelper(pid, spec) }, stop) {
			return
		}
		select {
		case <-stop:
			return
		default:
		}
		vm.launchMu.Lock()
		defer vm.launchMu.Unlock()
		vm.mu.Lock()
		if vm.HelperPID != pid || vm.stopping || vm.Status != "running" {
			vm.mu.Unlock()
			return
		}
		vm.mu.Unlock()
		e.unexpectedVMExit(vm, engine.LifecycleEvent{Kind: engine.VMExited, EngineID: vm.ID,
			SandboxID: vm.sandboxRef, UserID: vm.UserID, ExitCode: -1,
			Reason: "adopted bhatti-vmm exited (exit status unavailable: not this daemon's child)"})
	}()
}

func exitEvent(vm *VM, err error) engine.LifecycleEvent {
	event := engine.LifecycleEvent{Kind: engine.VMExited, EngineID: vm.ID,
		SandboxID: vm.sandboxRef, UserID: vm.UserID, ExitCode: -1}
	if err == nil {
		event.ExitCode = 0
		event.Reason = "bhatti-vmm exited with status 0"
		return event
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				event.Signal = status.Signal().String()
			} else {
				event.ExitCode = status.ExitStatus()
			}
		}
	}
	event.Reason = fmt.Sprintf("bhatti-vmm exited: %v", err)
	return event
}

// Caller holds launchMu. State is committed before notifying the server; a
// keep-hot restart in the handler can therefore safely boot on the same disks.
func (e *Engine) unexpectedVMExit(vm *VM, event engine.LifecycleEvent) {
	vm.mu.Lock()
	cancel := vm.cancel
	vm.cmd, vm.waitDone, vm.exitDone, vm.cancel = nil, nil, nil, nil
	vm.HelperPID, vm.Status, vm.Thermal = 0, "stopped", "cold"
	vm.Agent, vm.AgentInfoErr = nil, nil
	vm.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	vm.closeConfigSrv()
	e.delSandboxPolicy(vm)
	vm.removeSockets()
	vm.persist()
	e.emitLifecycle(event)
}

func (vm *VM) removeSockets() {
	for _, path := range []string{vm.ControlUDS, vm.ForwardUDS, vm.CtlSockUDS, vm.baseSpec.VsockConfigUDS} {
		if path != "" {
			_ = syscall.Unlink(path)
		}
	}
}
