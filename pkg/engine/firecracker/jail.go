//go:build linux

package firecracker

// jailPaths resolves file paths for Firecracker API calls.
// In bare mode, returns host paths as-is.
// In jailed mode, returns chroot-relative paths and tracks files
// that need to be hard-linked into the chroot before FC starts.
type jailPaths struct {
	jailed bool
	files  map[string]string // chroot filename → host path
}

func newJailPaths(jailed bool) *jailPaths {
	return &jailPaths{
		jailed: jailed,
		files:  make(map[string]string),
	}
}

// resolve maps a host path to the path FC should see.
// In bare mode: returns hostPath unchanged.
// In jailed mode: registers the file for hard-linking and returns
// the chroot-relative path (e.g. "/rootfs.ext4").
func (jp *jailPaths) resolve(chrootName, hostPath string) string {
	if !jp.jailed {
		return hostPath
	}
	jp.files[chrootName] = hostPath
	return "/" + chrootName
}
