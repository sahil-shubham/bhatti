package krucible

import "github.com/sahil-shubham/bhatti/pkg/engine"

// Mounts returns the host bind paths from the persisted launch spec. Server
// reauthorizes these against its current mount_roots before every new launch.
func (e *Engine) Mounts(id string) ([]engine.FsMount, bool) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, false
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	mounts := make([]engine.FsMount, len(vm.baseSpec.Mounts))
	for i, mount := range vm.baseSpec.Mounts {
		mounts[i] = engine.FsMount{HostPath: mount.HostPath, ReadOnly: mount.ReadOnly}
	}
	return mounts, true
}
