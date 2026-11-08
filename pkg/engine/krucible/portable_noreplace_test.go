package krucible

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLayeredImportWithoutHardlinksPreservesExistingBase(t *testing.T) {
	for _, linkErr := range []error{syscall.EXDEV, syscall.EOPNOTSUPP} {
		t.Run(linkErr.Error(), func(t *testing.T) {
			f, dir, m := layeredSnapshot(t)
			archive := exportArchive(t, f.e, dir, m, true)
			b := portableEngine(t, "", "exit 0")
			squatter := b.keptBasePath(fileSum(t, f.base), filepath.Base(f.base))
			original := writeBase(t, squatter, 17)
			oldLink, oldBaseLink := linkNode, linkBase
			t.Cleanup(func() { linkNode, linkBase = oldLink, oldBaseLink })
			links, baseLinks := 0, 0
			linkNode = func(src, dst string) error {
				links++
				return &os.LinkError{Op: "link", Old: src, New: dst, Err: linkErr}
			}
			linkBase = func(src, dst string) error {
				baseLinks++
				return &os.LinkError{Op: "link", Old: src, New: dst, Err: linkErr}
			}
			dest, _, importErr := importArchive(b, archive)
			got, err := os.ReadFile(squatter)
			if err != nil || !bytes.Equal(got, original) {
				t.Errorf("no-hardlink import replaced an existing base: %v", err)
			}
			if importErr == nil || !strings.Contains(importErr.Error(), "already holds another image") {
				t.Fatalf("no-hardlink base collision wasn't refused: %v", importErr)
			}
			if links == 0 || baseLinks != 1 {
				t.Fatalf("copy fallback wasn't exercised: links=%d baseLinks=%d", links, baseLinks)
			}
			mustNotExist(t, dest)
			if nodes := nodeEntries(t, b.cfg.DataDir); len(nodes) != 0 {
				t.Fatalf("no-hardlink base publication failure left nodes: %v", nodes)
			}
		})
	}
}
