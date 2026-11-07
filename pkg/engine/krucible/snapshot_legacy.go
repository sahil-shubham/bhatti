package krucible

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// Legacy inference must not turn the frozen source into a new trust record.
// Only new daemon-written copies receive explicit formats and rebased links.
// The caller holds the shared bases lock and sweeps unreferenced nodes on error.
func (e *Engine) normalizeLegacyDisk(dir, path, format string, chain []diskLayer) (normalizedPath string, normalizedChain []diskLayer, err error) {
	recordedErr := validateRecordedDisk(path, format, chain)
	if recordedErr == nil {
		return path, chain, nil
	}
	if format != "qcow2" || len(chain) > maxChainDepth {
		return "", nil, recordedErr
	}
	qcowCount := 1
	for i, layer := range chain {
		if layer.Path == "" {
			return "", nil, fmt.Errorf("legacy disk %s: empty backing path", path)
		}
		switch layer.Format {
		case "qcow2":
			qcowCount++
		case "raw":
			if i != len(chain)-1 {
				return "", nil, fmt.Errorf("legacy disk %s: raw backing has ancestors", path)
			}
		default:
			return "", nil, fmt.Errorf("legacy disk %s: unsupported backing format %q", path, layer.Format)
		}
	}
	if qcowCount > maxChainDepth {
		return "", nil, fmt.Errorf("legacy disk %s: more than %d qcow2 images", path, maxChainDepth)
	}
	needsNormalization := false
	for i := range qcowCount {
		source := path
		if i > 0 {
			source = chain[i-1].Path
		}
		var expected *diskLayer
		if i < len(chain) {
			expected = &chain[i]
		}
		headerFormat, relative, err := legacyDiskReference(source, source, expected)
		if err != nil {
			return "", nil, fmt.Errorf("legacy disk %s: %w", source, err)
		}
		needsNormalization = needsNormalization || relative || (expected != nil && headerFormat == "")
	}
	if !needsNormalization {
		return "", nil, recordedErr
	}

	normalized := slices.Clone(chain)
	var pending string
	defer func() {
		if pending != "" {
			if cleanupErr := os.Remove(pending); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove normalized disk stage: %w", cleanupErr))
			}
		}
	}()
	for i := qcowCount - 1; ; i-- {
		source := path
		if i > 0 {
			source = chain[i-1].Path
		}
		f, err := os.CreateTemp(dir, "disk-*.qcow2")
		if err != nil {
			return "", nil, err
		}
		copyPath := f.Name()
		pending = copyPath
		if err := f.Close(); err != nil {
			return "", nil, err
		}
		if err := cloneFile(source, copyPath); err != nil {
			return "", nil, err
		}
		if err := os.Chmod(copyPath, 0o600); err != nil {
			return "", nil, err
		}
		var expected *diskLayer
		if i < len(chain) {
			expected = &chain[i]
		}
		headerFormat, _, err := legacyDiskReference(copyPath, source, expected)
		if err != nil {
			return "", nil, fmt.Errorf("copied legacy disk %s: %w", source, err)
		}
		var backing, backingFormat string
		if i < len(normalized) {
			backing, backingFormat = normalized[i].Path, normalized[i].Format
		}
		if err := rewriteArchiveBacking(copyPath, backing, backingFormat, headerFormat); err != nil {
			return "", nil, fmt.Errorf("normalize legacy disk %s: %w", source, err)
		}
		if backing == "" {
			// An unchanged terminal header makes the rewriter a no-op, but
			// its cloned data must still be durable before node publication.
			f, err := os.OpenFile(copyPath, os.O_RDWR, 0)
			if err != nil {
				return "", nil, err
			}
			if err := errors.Join(f.Sync(), f.Close()); err != nil {
				return "", nil, err
			}
		}
		if i == 0 {
			if err := validateRecordedDisk(copyPath, "qcow2", normalized); err != nil {
				return "", nil, fmt.Errorf("normalized legacy disk: %w", err)
			}
			pending = ""
			return copyPath, normalized, nil
		}
		node, err := e.adoptNode(copyPath, "qcow2", normalized[i:])
		if err != nil {
			return "", nil, err
		}
		normalized[i-1].Path = node
		if err := os.Remove(copyPath); err != nil {
			return "", nil, err
		}
		pending = ""
	}
}

func legacyDiskReference(path, source string, expected *diskLayer) (headerFormat string, relative bool, err error) {
	name, err := qcow2Backing(path)
	if err != nil {
		return "", false, err
	}
	headerFormat, err = qcow2BackingFormat(path)
	if err != nil {
		return "", false, err
	}
	next := name
	relative = name != "" && !filepath.IsAbs(name)
	if relative {
		// A clone still carries the source's reference until it is rebased.
		next = filepath.Join(filepath.Dir(source), name)
	}
	if expected == nil {
		if next != "" {
			return "", false, fmt.Errorf("unrecorded backing path %s", next)
		}
	} else if next != expected.Path || (headerFormat != "" && headerFormat != expected.Format) {
		return "", false, fmt.Errorf("backing %s (%s) does not match authorized backing %s (%s)", next, headerFormat, expected.Path, expected.Format)
	}
	return headerFormat, relative, nil
}
