package krucible

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestForkLayeredNamedOwnerWithoutNICWithNodeDAC(t *testing.T) {
	e := rootReseedStorageEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	marker := filepath.Join(e.cfg.DataDir, "netd-started")
	netd := filepath.Join(e.cfg.DataDir, "must-not-start-netd")
	writeFile(t, netd, []byte(fmt.Sprintf("#!/bin/sh\nprintf started > %q\nexit 2\n", marker)))
	if err := os.Chmod(netd, 0o755); err != nil {
		t.Fatal(err)
	}
	e.cfg.NetdBinary = netd
	spec := noNetworkSpec("source")
	spec.UserID = "named-owner"
	source, err := e.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(e.cfg.DataDir, "snapshots", spec.UserID)
	if _, err := e.Checkpoint(ctx, source.ID, spec.UserID, 0, "seed", parent); err != nil {
		t.Fatal(err)
	}
	layered, err := e.ResumeFromManifestJSON(ctx, filepath.Join(parent, "seed"), nil, "layered", spec.UserID)
	if err != nil {
		t.Fatal(err)
	}
	previous := layered.ID
	for _, name := range []string{"fork", "fork-of-fork"} {
		child, err := e.Fork(ctx, previous, name)
		if err != nil {
			t.Fatalf("%s of layered named-owner sandbox without a NIC: %v", name, err)
		}
		vm, err := e.getVM(child.ID)
		if err != nil {
			t.Fatal(err)
		}
		vm.mu.Lock()
		owner, netdKey, netIP := vm.UserID, vm.netdKey, vm.netIP
		backing := len(vm.baseSpec.Backing)
		vm.mu.Unlock()
		if owner != spec.UserID || netdKey != "" || netIP != "" || backing < 2 {
			t.Fatalf("%s lost storage owner or gained networking: owner=%q netd=%q ip=%q depth=%d", name, owner, netdKey, netIP, backing)
		}
		previous = child.ID
	}
	if len(e.netds) != 0 {
		t.Fatalf("no-NIC lineage registered netds: %+v", e.netds)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-NIC lineage started netd: %v", err)
	}
}
