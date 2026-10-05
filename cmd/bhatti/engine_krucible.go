package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

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

	// bhatti-netd: the per-owner network gateway, the guest's only network.
	// Autodetected next to the binary / on PATH (same discovery as vmm); the
	// engine refuses to start without it.
	if cfg.KrucibleNetBackend != nil && !*cfg.KrucibleNetBackend {
		return nil, fmt.Errorf("krucible_net_backend: false is no longer supported: " +
			"guests are networked only through bhatti-netd; remove the key")
	}
	netd := cfg.KrucibleNetd
	if netd == "" {
		if exe, err := os.Executable(); err == nil {
			cand := filepath.Join(filepath.Dir(exe), "bhatti-netd")
			if _, err := os.Stat(cand); err == nil {
				netd = cand
			}
		}
		if netd == "" {
			if p, err := exec.LookPath("bhatti-netd"); err == nil {
				netd = p
			}
		}
	}

	// Where libkrun lives when bhatti-vmm can't find it through its rpath (dev
	// builds); put on the helper's library path.
	libDir := cfg.KrucibleLibDir

	// A prebuilt base image implies the block-root (cold-capable) path.
	blockRoot := cfg.KrucibleBlockRoot || cfg.KrucibleBaseImage != ""

	// Lean external kernel (bhatti-vmm boots nothing else). Explicit config wins;
	// else autodetect a dist/kernel/{Image,vmlinux}-lean-*-<arch> next to the
	// binary or in the CWD. The engine refuses to start without one.
	kernelImage := cfg.KrucibleKernelImage
	if kernelImage == "" && blockRoot {
		karch := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[runtime.GOARCH]
		var dirs []string
		if exe, err := os.Executable(); err == nil {
			dirs = append(dirs, filepath.Join(filepath.Dir(exe), "dist", "kernel"))
		}
		dirs = append(dirs, "dist/kernel")
		for _, d := range dirs {
			for _, pat := range []string{"Image-lean-*-" + karch, "vmlinux-lean-*-" + karch} {
				if m, _ := filepath.Glob(filepath.Join(d, pat)); len(m) > 0 {
					kernelImage = m[0]
					break
				}
			}
			if kernelImage != "" {
				break
			}
		}
	}

	daemonListen := []string{cfg.Listen, cfg.PublicProxyListen}
	if cfg.Domain != nil {
		// Domain mode listens on wildcard :443/:80; netd also snapshots every
		// host interface address so either listener stays unreachable to guests.
		daemonListen = []string{":443", ":80"}
	}

	return krucible.New(krucible.Config{
		DataDir:      cfg.DataDir,
		BaseRootfs:   cfg.KrucibleRootfs,
		BaseImage:    cfg.KrucibleBaseImage,
		BlockRoot:    blockRoot,
		VMMBinary:    vmm,
		LibDir:       libDir,
		SocketDir:    cfg.KrucibleSocketDir,
		KernelImage:  kernelImage,
		NetdBinary:   netd,
		DaemonListen: daemonListen,
	})
}
