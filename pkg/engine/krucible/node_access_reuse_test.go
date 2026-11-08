package krucible

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

type observedNodeInfo struct {
	os.FileInfo
	mode os.FileMode
	stat syscall.Stat_t
}

func (f *observedNodeInfo) Mode() os.FileMode { return f.mode }
func (f *observedNodeInfo) Sys() any          { return &f.stat }

type observedNodeFile struct {
	info    observedNodeInfo
	changes []string
}

func (f *observedNodeFile) Stat() (os.FileInfo, error) { return &f.info, nil }
func (f *observedNodeFile) Chmod(mode os.FileMode) error {
	f.changes = append(f.changes, fmt.Sprintf("chmod %04o", mode))
	f.info.mode = mode
	return nil
}
func (f *observedNodeFile) Chown(uid, gid int) error {
	f.changes = append(f.changes, fmt.Sprintf("chown %d:%d", uid, gid))
	f.info.stat.Uid, f.info.stat.Gid = uint32(uid), uint32(gid)
	return nil
}
func (f *observedNodeFile) Sync() error {
	f.changes = append(f.changes, "sync")
	return nil
}

func TestNodeAccessReuseNeverRevokesReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node")
	writeFile(t, path, []byte("private"))
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	const gid = vmmNodeGIDBase
	f := &observedNodeFile{info: observedNodeInfo{FileInfo: fi, mode: 0o640, stat: syscall.Stat_t{Gid: gid}}}
	for range 3 {
		if err := applyNodeAccess(f, path, gid, true); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.changes) != 0 {
		t.Fatalf("already-correct node changed during repeated preparation: %v", f.changes)
	}
	// Correcting only the mode must not insert a 0600 window either.
	f.info.mode = 0o644
	if err := applyNodeAccess(f, path, gid, true); err != nil {
		t.Fatal(err)
	}
	if want := []string{"chmod 0640", "sync"}; !reflect.DeepEqual(f.changes, want) {
		t.Fatalf("same-group mode correction: %v, want %v", f.changes, want)
	}
	// A real group transition must revoke the old reader before reassignment.
	f.changes = nil
	if err := applyNodeAccess(f, path, gid+1, true); err != nil {
		t.Fatal(err)
	}
	want := []string{"chmod 0600", fmt.Sprintf("chown 0:%d", gid+1), "chmod 0640", "sync"}
	if !reflect.DeepEqual(f.changes, want) {
		t.Fatalf("group transition: %v, want %v", f.changes, want)
	}
}
