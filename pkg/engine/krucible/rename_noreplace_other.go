//go:build !linux && !darwin

package krucible

import "errors"

func renameNoReplace(src, dst string) error {
	return errors.New("non-replacing rename is unsupported on this platform")
}
