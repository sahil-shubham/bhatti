package main

import (
	"log/slog"
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

	// bhatti-netd: the per-owner network gateway (pure Go), the DEFAULT in v2.
	// Autodetected next to the binary / on PATH (same discovery as vmm). If the
	// net backend is on but the helper is missing, fail fast with a clear message
	// rather than silently falling back to the insecure shared-netstack (TSI).
	netd := cfg.KrucibleNetd
	if netd == "" && cfg.KrucibleNetBackend {
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
	netBackend := cfg.KrucibleNetBackend
	if netBackend && netd == "" {
		// The secure gateway is the default, but the daemon is built at startup on
		// hosts that may not have the runtime (dev builds, the unit-test gate). Don't
		// refuse to start — fall back to TSI with a LOUD warning. A correct install
		// ships bhatti-netd next to the binary, so this never fires in production.
		slog.Warn("bhatti-netd not found — the secure network gateway is DISABLED and the " +
			"guest is NOT isolated from the host (legacy TSI). Install the runtime bundle or run " +
			"`make netd`; set krucible_net_backend: false to silence this warning.")
		netBackend = false
	}

	libDir := cfg.KrucibleLibDir
	if libDir == "" {
		// libkrunfw lives in Homebrew on macOS, /usr/local/lib64 (or lib) on Linux.
		for _, d := range []string{"/opt/homebrew/lib", "/usr/local/lib64", "/usr/local/lib", "/usr/lib64", "/usr/lib"} {
			if m, _ := filepath.Glob(filepath.Join(d, "libkrunfw*")); len(m) > 0 {
				libDir = d
				break
			}
		}
	}

	// A prebuilt base image implies the block-root (cold-capable) path.
	blockRoot := cfg.KrucibleBlockRoot || cfg.KrucibleBaseImage != ""

	// Lean external kernel (krucible external-kernel boot, ~2x faster cold-start
	// than libkrunfw's bundled kernel). Explicit config wins; else autodetect a
	// dist/kernel/{Image,vmlinux}-lean-*-<arch> next to the binary or in the CWD.
	// Empty -> fall back to the libkrunfw bundle. Block-root only (engine-gated).
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

	return krucible.New(krucible.Config{
		DataDir:     cfg.DataDir,
		BaseRootfs:  cfg.KrucibleRootfs,
		BaseImage:   cfg.KrucibleBaseImage,
		BlockRoot:   blockRoot,
		VMMBinary:   vmm,
		LibDir:      libDir,
		SocketDir:   cfg.KrucibleSocketDir,
		KernelImage: kernelImage,
		NetBackend:  netBackend,
		NetdBinary:  netd,
	})
}
