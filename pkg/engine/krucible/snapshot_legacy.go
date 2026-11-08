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
func (e *Engine) normalizeLegacyDisk(dir, path, format string, chain []diskLayer) (string, []diskLayer, error) {
	if err := validateRecordedDisk(path, format, chain); err == nil {
		return path, chain, nil
	}
	if _, err := validateLegacyDisk(path, path, format, chain); err != nil {
		return "", nil, err
	}
	copyPath, err := copyLegacyDisk(dir, path)
	if err != nil {
		return "", nil, err
	}
	normalized, err := e.normalizeLegacyDiskCopy(copyPath, path, format, chain)
	if err != nil {
		return "", nil, errors.Join(err, os.Remove(copyPath))
	}
	return copyPath, normalized, nil
}

// The authorized baseline supplies paths and inferred drivers, but missing
// optional extensions remain acceptable only while comparing legacy sources.
func validateLegacyDisk(path, source, format string, chain []diskLayer) (int, error) {
	if format != "qcow2" {
		return 0, validateRecordedDisk(path, format, chain)
	}
	if chain == nil || len(chain) > maxChainDepth {
		return 0, fmt.Errorf("legacy disk %s: missing or excessive authorized backing chain", path)
	}
	qcowCount := 1
	for i, layer := range chain {
		if layer.Path == "" {
			return 0, fmt.Errorf("legacy disk %s: empty backing path", path)
		}
		switch layer.Format {
		case "qcow2":
			qcowCount++
		case "raw":
			if i != len(chain)-1 {
				return 0, fmt.Errorf("legacy disk %s: raw backing has ancestors", path)
			}
		default:
			return 0, fmt.Errorf("legacy disk %s: unsupported backing format %q", path, layer.Format)
		}
	}
	if qcowCount > maxChainDepth {
		return 0, fmt.Errorf("legacy disk %s: more than %d qcow2 images", path, maxChainDepth)
	}
	for i := range qcowCount {
		current, origin := path, source
		if i > 0 {
			current = chain[i-1].Path
			origin = current
		}
		var expected *diskLayer
		if i < len(chain) {
			expected = &chain[i]
		}
		if _, _, err := legacyDiskReference(current, origin, expected); err != nil {
			return 0, fmt.Errorf("legacy disk %s: %w", current, err)
		}
	}
	return qcowCount, nil
}

// path is already a private copy. Leave strict ancestors alone; copy only
// ancestors needing an explicit format, an absolute pointer, or a new child.
func (e *Engine) normalizeLegacyDiskCopy(path, source, format string, chain []diskLayer) (normalized []diskLayer, err error) {
	qcowCount, err := validateLegacyDisk(path, source, format, chain)
	if err != nil {
		return nil, err
	}
	if err := validateRecordedDisk(path, format, chain); err == nil {
		return chain, nil
	}
	normalized = chain
	changed := false
	var pending string
	defer func() {
		if pending != "" {
			if cleanupErr := os.Remove(pending); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove normalized disk stage: %w", cleanupErr))
			}
		}
	}()
	for i := qcowCount - 1; i > 0; i-- {
		ancestor := chain[i-1].Path
		var expected *diskLayer
		var backing, backingFormat string
		if i < len(chain) {
			expected = &chain[i]
			backing, backingFormat = normalized[i].Path, normalized[i].Format
		}
		headerFormat, relative, err := legacyDiskReference(ancestor, ancestor, expected)
		if err != nil {
			return nil, fmt.Errorf("legacy disk %s: %w", ancestor, err)
		}
		if !relative && (expected == nil || (headerFormat == backingFormat && expected.Path == backing)) {
			continue
		}
		copyPath, err := copyLegacyDisk(filepath.Dir(path), ancestor)
		if err != nil {
			return nil, err
		}
		pending = copyPath
		headerFormat, _, err = legacyDiskReference(copyPath, ancestor, expected)
		if err != nil {
			return nil, fmt.Errorf("copied legacy disk %s: %w", ancestor, err)
		}
		if err := rewriteArchiveBacking(copyPath, backing, backingFormat, headerFormat); err != nil {
			return nil, fmt.Errorf("normalize legacy disk %s: %w", ancestor, err)
		}
		node, err := e.adoptNode(copyPath, "qcow2", normalized[i:])
		if err != nil {
			return nil, err
		}
		if !changed {
			normalized = slices.Clone(chain)
			changed = true
		}
		normalized[i-1].Path = node
		if err := os.Remove(copyPath); err != nil {
			return nil, err
		}
		pending = ""
	}
	var expected *diskLayer
	var backing, backingFormat string
	if len(chain) > 0 {
		expected = &chain[0]
		backing, backingFormat = normalized[0].Path, normalized[0].Format
	}
	headerFormat, _, err := legacyDiskReference(path, source, expected)
	if err != nil {
		return nil, fmt.Errorf("copied legacy disk %s: %w", source, err)
	}
	if err := rewriteArchiveBacking(path, backing, backingFormat, headerFormat); err != nil {
		return nil, fmt.Errorf("normalize legacy disk %s: %w", source, err)
	}
	if err := validateRecordedDisk(path, format, normalized); err != nil {
		return nil, fmt.Errorf("normalized legacy disk: %w", err)
	}
	return normalized, nil
}

func copyLegacyDisk(dir, source string) (string, error) {
	f, err := os.CreateTemp(dir, "disk-*.qcow2")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		return "", errors.Join(err, os.Remove(path))
	}
	if err := cloneFile(source, path); err != nil {
		return "", errors.Join(err, os.Remove(path))
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", errors.Join(err, os.Remove(path))
	}
	return path, nil
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
