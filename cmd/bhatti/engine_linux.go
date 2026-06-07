//go:build linux

package main

import (
	"github.com/sahil-shubham/bhatti/pkg"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	fc "github.com/sahil-shubham/bhatti/pkg/engine/firecracker"
)

func newFirecrackerEngine(cfg *pkg.Config) (engine.Engine, error) {
	return fc.New(fc.Config{
		DataDir:      cfg.DataDir,
		KernelPath:   cfg.FirecrackerKernel,
		BaseRootfs:   cfg.FirecrackerRootfs,
		FCBinary:     cfg.FirecrackerBin,
		JailerBinary: cfg.FirecrackerJailer,
		JailUID:      cfg.JailUID,
		JailGID:      cfg.JailGID,
		DNSUpstreams: cfg.DNSUpstreams, // empty → engine default (1.1.1.1/8.8.8.8)
	})
}
