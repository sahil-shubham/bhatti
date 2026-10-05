package krucible

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

// The daemon walks its data dir for images (MigrateImages at startup, image
// GC) while holding state.db open. If that walk opens and closes state.db's
// WAL or shm, the daemon's SQLite POSIX locks vanish, and the next process to
// open the database (a `bhatti user` command) deletes the live WAL on close:
// from then on the two write into different WALs and the database corrupts.
func TestImageWalkKeepsDaemonStoreLocks(t *testing.T) {
	if db := os.Getenv("BHATTI_TEST_STORE_WRITER"); db != "" {
		st, err := store.New(db)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.CreateUser(store.User{ID: "usr_cli", Name: "cli", APIKeyHash: "cli-hash", SubnetIndex: 2, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		st.Close()
		return
	}
	data := t.TempDir()
	db := filepath.Join(data, "state.db")
	daemon, err := store.New(db)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	if _, err := daemon.ListUsers(); err != nil {
		t.Fatal(err)
	}
	writeBase(t, filepath.Join(data, "images", "rootfs-minimal-"+runtime.GOARCH+".ext4"), 1)
	if _, err := MigrateImages(data); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestImageWalkKeepsDaemonStoreLocks$")
	cmd.Env = append(os.Environ(), "BHATTI_TEST_STORE_WRITER="+db)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("writer process: %v\n%s", err, out)
	}
	if _, err := os.Stat(db + "-wal"); err != nil {
		t.Fatalf("a second process deleted the daemon's live WAL: %v", err)
	}
	if _, err := daemon.GetUserByKeyHash("cli-hash"); err != nil {
		t.Fatalf("daemon cannot see the other process's commit: %v", err)
	}
}
