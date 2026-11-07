//go:build !darwin

package collector

import "errors"

// processReader exists so the collector builds everywhere. Only macOS measures
// a helper per process: on Linux a helper is measured through its cgroup, or not
// at all.
type processReader struct{}

func newProcessReader() *processReader { return &processReader{} }

func (*processReader) usage(int) (Usage, error) {
	return Usage{}, errors.New("per-process figures are only read on macOS")
}
