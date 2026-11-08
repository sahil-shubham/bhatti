//go:build linux

package krucible

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartupReconcilesNodePublicationBeforeAccessMigration(t *testing.T) {
	const childEnv = "BHATTI_TEST_STARTUP_PUBLICATION"
	if source := os.Getenv(childEnv); source != "" {
		data := os.Getenv("BHATTI_TEST_STARTUP_DATA")
		e := &Engine{cfg: Config{DataDir: data}}
		chain, err := readDiskChain(source, "qcow2", nil)
		if err != nil {
			t.Fatal(err)
		}
		syncNodeDir = func(dir string) error { os.Exit(93); return nil }
		_, err = e.linkNodeMetadata(source, "qcow2", chain)
		t.Fatalf("publication did not stop at its post-link directory barrier: %v", err)
	}
	if os.Geteuid() != 0 {
		t.Skip("requires root Linux startup confinement")
	}
	f := newNodeFixture(t)
	source := f.freshRoot("source")
	cmd := exec.Command(os.Args[0], "-test.run=^TestStartupReconcilesNodePublicationBeforeAccessMigration$")
	cmd.Env = append(os.Environ(), childEnv+"="+source, "BHATTI_TEST_STARTUP_DATA="+f.data)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 93 {
		t.Fatalf("crash boundary = %v: %s", err, out)
	}
	pins, err := filepath.Glob(filepath.Join(nodesDir(f.data), ".publish-*"))
	if err != nil || len(pins) != 2 {
		t.Fatalf("crash left no data pin and authority pair: %v, %v", pins, err)
	}
	eng, err := New(storageStartupConfig(t, f.data, f.base))
	if err != nil {
		t.Fatalf("production startup rejected recoverable publication: %v", err)
	}
	if !eng.dropVMM {
		t.Fatal("startup bypassed root-Linux permission migration")
	}
	for _, pin := range pins {
		if _, err := os.Lstat(pin); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("startup retained publication stage %s: %v", pin, err)
		}
	}
	entries, err := os.ReadDir(nodesDir(f.data))
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range entries {
		if strings.HasPrefix(de.Name(), ".") {
			continue
		}
		var metadata nodeMetadata
		if found, err := readDaemonMetadata(nodeMetadataPath(filepath.Join(nodesDir(f.data), de.Name())), &metadata); err != nil || !found {
			t.Fatalf("startup lost the published graph: %t, %v", found, err)
		}
	}
}
