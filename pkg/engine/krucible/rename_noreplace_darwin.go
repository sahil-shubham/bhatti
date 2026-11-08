//go:build darwin

package krucible

import "golang.org/x/sys/unix"

func renameNoReplace(src, dst string) error {
	return unix.RenamexNp(src, dst, unix.RENAME_EXCL)
}
