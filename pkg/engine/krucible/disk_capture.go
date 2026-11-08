package krucible

import (
	"context"
	"fmt"
	"slices"
)

// Inferred chains belong to this capture, not to the running VM's authority.
// Typed captures need neither a volume copy nor per-disk legacy allocations.
type diskCaptureBaseline struct {
	rootLegacy   bool
	volumeLegacy []bool
}

func (b diskCaptureBaseline) legacyVolume(i int) bool {
	return len(b.volumeLegacy) > i && b.volumeLegacy[i]
}

func (b diskCaptureBaseline) legacy() bool {
	return b.rootLegacy || len(b.volumeLegacy) != 0
}

func (e *Engine) captureDiskBacking(vm *VM, spec *VMSpec) (diskCaptureBaseline, error) {
	var baseline diskCaptureBaseline
	// An empty VM driver already means raw, even if its bytes resemble qcow2.
	if spec.RootDiskFormat == "" {
		spec.RootDiskFormat = "raw"
	}
	baseline.rootLegacy = spec.RootDiskFormat == "qcow2" && spec.Backing == nil
	if baseline.rootLegacy {
		chain, err := e.legacyDiskChain(vm, spec.RootDisk, spec.RootDiskFormat)
		if err != nil {
			return baseline, fmt.Errorf("root disk: %w", err)
		}
		spec.Backing = chain
	}
	for i, v := range spec.Volumes {
		if v.Format != "qcow2" || v.Backing != nil {
			continue
		}
		chain, err := e.legacyDiskChain(vm, v.Path, v.Format)
		if err != nil {
			return baseline, fmt.Errorf("volume %s: %w", v.BlockID, err)
		}
		if baseline.volumeLegacy == nil {
			spec.Volumes = slices.Clone(spec.Volumes)
			baseline.volumeLegacy = make([]bool, len(spec.Volumes))
		}
		baseline.volumeLegacy[i] = true
		spec.Volumes[i].Backing = chain
	}
	return baseline, baseline.validate(*spec)
}

func (b diskCaptureBaseline) validate(spec VMSpec) error {
	if err := validateCaptureDisk(spec.RootDisk, spec.RootDisk, spec.RootDiskFormat, spec.Backing, b.rootLegacy); err != nil {
		return fmt.Errorf("root disk: %w", err)
	}
	for i, v := range spec.Volumes {
		if err := validateCaptureDisk(v.Path, v.Path, v.Format, v.Backing, b.legacyVolume(i)); err != nil {
			return fmt.Errorf("volume %s: %w", v.BlockID, err)
		}
	}
	return nil
}

func validateCaptureDisk(path, source, format string, chain []diskLayer, legacy bool) error {
	if legacy {
		_, err := validateLegacyDisk(path, source, format, chain)
		return err
	}
	return validateRecordedDisk(path, format, chain)
}

// Only a daemon-owned captured disk may gain explicit formats or rebased links.
// A relative top pointer still resolves from the source, not the capture dir.
func (e *Engine) normalizeCapturedDisk(ctx context.Context, path, source, format string, chain []diskLayer, legacy bool) ([]diskLayer, error) {
	if !legacy {
		return chain, validateRecordedDisk(path, format, chain)
	}
	return e.normalizeLegacyDiskCopy(ctx, path, source, format, chain)
}
