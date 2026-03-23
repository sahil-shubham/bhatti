package oci

import (
	"archive/tar"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// extractLayer extracts a single OCI layer tar into the target directory.
// Uses a staging directory + whiteout application to correctly handle
// OCI whiteout semantics including opaque whiteouts in any order.
func extractLayer(layer v1.Layer, targetDir string) error {
	stageDir, err := os.MkdirTemp("", "bhatti-layer-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stageDir)

	reader, err := layer.Uncompressed()
	if err != nil {
		return err
	}
	defer reader.Close()

	tr := tar.NewReader(reader)

	type whiteout struct {
		path   string // relative path
		opaque bool
	}
	var whiteouts []whiteout

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Normalize name (remove leading ./ or /)
		headerName := strings.TrimPrefix(header.Name, "./")
		headerName = strings.TrimPrefix(headerName, "/")
		if headerName == "" {
			continue
		}

		base := filepath.Base(headerName)

		// Collect whiteout entries
		if base == ".wh..wh..opq" {
			whiteouts = append(whiteouts, whiteout{
				path: filepath.Dir(headerName), opaque: true,
			})
			continue
		}
		if strings.HasPrefix(base, ".wh.") {
			deletedName := strings.TrimPrefix(base, ".wh.")
			whiteouts = append(whiteouts, whiteout{
				path:   filepath.Join(filepath.Dir(headerName), deletedName),
				opaque: false,
			})
			continue
		}

		// Path traversal protection
		fullPath := filepath.Join(stageDir, headerName)
		if !strings.HasPrefix(filepath.Clean(fullPath)+string(os.PathSeparator), filepath.Clean(stageDir)+string(os.PathSeparator)) &&
			filepath.Clean(fullPath) != filepath.Clean(stageDir) {
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(fullPath, os.FileMode(header.Mode)|0700)
		case tar.TypeReg:
			if err := writeFile(fullPath, tr, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			os.MkdirAll(filepath.Dir(fullPath), 0755)
			os.Remove(fullPath) // remove any existing
			os.Symlink(header.Linkname, fullPath)
		case tar.TypeLink:
			linkTarget := filepath.Join(stageDir, strings.TrimPrefix(header.Linkname, "./"))
			os.MkdirAll(filepath.Dir(fullPath), 0755)
			os.Remove(fullPath)
			os.Link(linkTarget, fullPath)
		case tar.TypeBlock, tar.TypeChar:
			continue // skip device nodes
		default:
			continue
		}

		// Preserve ownership
		os.Lchown(fullPath, header.Uid, header.Gid)
	}

	// Apply whiteouts to target (deletes from previous layers)
	for _, wo := range whiteouts {
		target := filepath.Join(targetDir, wo.path)
		if wo.opaque {
			removeDirectoryContents(target)
		} else {
			os.RemoveAll(target)
		}
	}

	// Merge staged files into target — file-level walk, not directory rename
	return mergeDir(stageDir, targetDir)
}

func writeFile(path string, r io.Reader, mode os.FileMode) error {
	os.MkdirAll(filepath.Dir(path), 0755)
	if mode == 0 {
		mode = 0644
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// removeDirectoryContents removes all entries in a directory but not the directory itself.
func removeDirectoryContents(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// mergeDir walks src and copies/moves each file into dst.
// Directories are created (not replaced). Files are moved (O(1) on same FS).
func mergeDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip errors
		}
		relPath, _ := filepath.Rel(src, path)
		if relPath == "." {
			return nil
		}
		targetPath := filepath.Join(dst, relPath)

		if d.IsDir() {
			info, _ := d.Info()
			mode := os.FileMode(0755)
			if info != nil {
				mode = info.Mode()
			}
			os.MkdirAll(targetPath, mode)
			return nil
		}

		// For symlinks, os.Rename works but we need special handling
		// because d.Type() tells us if it's a symlink
		os.MkdirAll(filepath.Dir(targetPath), 0755)
		os.Remove(targetPath) // remove old file if exists

		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return nil
			}
			return os.Symlink(link, targetPath)
		}

		// Regular file — try rename (O(1) on same FS), fall back to copy
		if err := os.Rename(path, targetPath); err != nil {
			return copyFile(path, targetPath)
		}
		return nil
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, _ := in.Stat()
	mode := os.FileMode(0644)
	if info != nil {
		mode = info.Mode()
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}
