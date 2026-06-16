package main

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/sahil-shubham/bhatti/pkg"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/engine/krucible"
)

// newKrucibleEngine builds the libkrun-backed engine. Pure Go — it spawns the
// cgo bhatti-vmm helper, so this compiles and runs on macOS and Linux. The
// helper + libs are autodetected when not set in config.
func newKrucibleEngine(cfg *pkg.Config) (engine.Engine, error) {
	vmm := cfg.KrucibleVMM
	if vmm == "" {
		if exe, err := os.Executable(); err == nil {
			cand := filepath.Join(filepath.Dir(exe), "bhatti-vmm")
			if _, err := os.Stat(cand); err == nil {
				vmm = cand
			}
		}
		if vmm == "" {
			if p, err := exec.LookPath("bhatti-vmm"); err == nil {
				vmm = p
			}
		}
	}

	libDir := cfg.KrucibleLibDir
	if libDir == "" {
		for _, d := range []string{"/opt/homebrew/lib", "/usr/local/lib", "/usr/lib"} {
			if m, _ := filepath.Glob(filepath.Join(d, "libkrunfw*")); len(m) > 0 {
				libDir = d
				break
			}
		}
	}

	// A prebuilt base image implies the block-root (cold-capable) path.
	blockRoot := cfg.KrucibleBlockRoot || cfg.KrucibleBaseImage != ""

	return krucible.New(krucible.Config{
		DataDir:    cfg.DataDir,
		BaseRootfs: cfg.KrucibleRootfs,
		BaseImage:  cfg.KrucibleBaseImage,
		BlockRoot:  blockRoot,
		VMMBinary:  vmm,
		LibDir:     libDir,
		SocketDir:  cfg.KrucibleSocketDir,
	})
}
