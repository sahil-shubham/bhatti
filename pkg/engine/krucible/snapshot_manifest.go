package krucible

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/sahil-shubham/bhatti/pkg/configdrive"
)

const snapshotManifestFile = "manifest.json"

// snapshotBootFile holds a memory snapshot's guestBoot: daemon metadata like
// the manifest, readable by the daemon alone (its env can hold credentials a
// user passed in). Like the manifest it stays out of exports, whose archive
// has no member for it yet, so a copy resumed from an imported snapshot
// cold-boots without its source's env, files and --init.
const snapshotBootFile = "boot.json"

// guestBoot is what a memory copy's boot config keeps from its source: the
// settings its restored guest came back with, which lohar applies again at
// every cold boot. The copy's identity, network, devices and CA are its own
// (the server sets the CA it inherits with SetCACert).
type guestBoot struct {
	Env   map[string]string                 `json:"env,omitempty"`
	Files map[string]configdrive.ConfigFile `json:"files,omitempty"`
	Init  string                            `json:"init,omitempty"`
}

// The local manifest is the daemon's disk-format and provenance record. It is
// published only after the copied disks or imported graph have been validated;
// the SQLite copy is an API record, not an alternative source of disk authority.
func writeSnapshotManifest(dir string, m krucibleSnapManifest) error {
	return writeDaemonMetadata(filepath.Join(dir, snapshotManifestFile), m)
}

func writeDaemonMetadata(path string, value any) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".manifest-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := json.NewEncoder(f).Encode(value); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func readSnapshotManifest(dir string) (m krucibleSnapManifest, found bool, err error) {
	found, err = readDaemonMetadata(filepath.Join(dir, snapshotManifestFile), &m)
	return m, found, err
}

func readDaemonMetadata(path string, value any) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	// No writable aliases: a helper's hard link must not be able to change
	// this record after the daemon has approved its contents.
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || !ok || st.Nlink != 1 || st.Uid != uint32(os.Geteuid()) || fi.Mode().Perm()&0o022 != 0 {
		return false, fmt.Errorf("%s is not a daemon-owned metadata record", path)
	}
	if err := json.NewDecoder(f).Decode(value); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}
