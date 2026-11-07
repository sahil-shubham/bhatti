//go:build !linux

package krucible

// hostCgroups is a host without cgroups, macOS here: nothing can be placed, so
// none of the Linux checks apply (resolveCgroups stops at the platform) and
// sandboxes can only be observed per process.
func hostCgroups() cgroupHost { return cgroupHost{} }
