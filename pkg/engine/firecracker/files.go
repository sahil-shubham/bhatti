//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"io"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)


func (e *Engine) Tunnel(ctx context.Context, id string, port int) (io.ReadWriteCloser, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, err
	}

	// Capture agent ref under lock, release before long-lived Tunnel call.
	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return nil, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	return ag.Forward(ctx, uint16(port))
}


// --- File Operations ---

func (e *Engine) FileRead(ctx context.Context, id, path string, w io.Writer, opts ...agent.FileReadOpts) (int64, string, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return 0, "", err
	}

	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return 0, "", fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	return ag.FileRead(ctx, path, w, opts...)
}

func (e *Engine) FileWrite(ctx context.Context, id, path, mode string, size int64, r io.Reader) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}

	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	return ag.FileWrite(ctx, path, mode, size, r)
}

func (e *Engine) FileStat(ctx context.Context, id, path string) (*proto.FileInfo, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, err
	}

	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return nil, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	return ag.FileStat(ctx, path)
}

func (e *Engine) FileList(ctx context.Context, id, path string) ([]proto.FileInfo, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, err
	}

	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return nil, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	return ag.FileList(ctx, path)
}

