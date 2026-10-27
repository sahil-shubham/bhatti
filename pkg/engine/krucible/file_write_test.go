//go:build krucible

package krucible

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// patternReader yields n bytes of a cheap repeating non-zero pattern.
type patternReader struct{ left int64 }

func (p *patternReader) Read(b []byte) (int, error) {
	if p.left == 0 {
		return 0, io.EOF
	}
	if int64(len(b)) > p.left {
		b = b[:p.left]
	}
	for i := range b {
		b[i] = byte(p.left+int64(i)) * 31
	}
	p.left -= int64(len(b))
	return len(b), nil
}

// A write above the old 100 MB cap lands intact, and one the disk cannot hold
// is refused up front with the typed error and its numbers, leaving the
// sandbox usable.
func TestKrucibleFileWriteBeyondOldCapAndNoSpace(t *testing.T) {
	eng := newBlockRootEngine(t)
	ke := eng.(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "bigwrite", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), info.ID) })

	const size = 189 << 20
	want := sha256.New()
	start := time.Now()
	if err := ke.FileWrite(ctx, info.ID, "/workspace/big.bin", "0644", size, io.TeeReader(&patternReader{left: size}, want)); err != nil {
		t.Fatalf("FileWrite %d bytes: %v", size, err)
	}
	t.Logf("wrote %d MB in %s", size>>20, time.Since(start).Round(time.Millisecond))
	got := sha256.New()
	n, _, err := ke.FileRead(ctx, info.ID, "/workspace/big.bin", got)
	if err != nil || n != size {
		t.Fatalf("FileRead = %d bytes, %v; want %d", n, err, size)
	}
	if g, w := hex.EncodeToString(got.Sum(nil)), hex.EncodeToString(want.Sum(nil)); g != w {
		t.Fatalf("sha256 read back %s, wrote %s", g, w)
	}

	// 1 TiB cannot fit: refused before the body is consumed.
	err = ke.FileWrite(ctx, info.ID, "/workspace/huge.bin", "0644", 1<<40, &patternReader{left: 1 << 40})
	if !errors.Is(err, agent.ErrNoSpace) || !strings.HasPrefix(err.Error(), "no space: need ") {
		t.Fatalf("1 TiB write = %v; want agent.ErrNoSpace with the guest's message", err)
	}
	t.Logf("refusal: %v", err)
	if _, err := ke.FileStat(ctx, info.ID, "/workspace/big.bin"); err != nil {
		t.Fatalf("sandbox unusable after refused write: %v", err)
	}
}

// Several uploads that each fit when checked but not together: the loser fails
// with ENOSPC mid-stream. That must surface as the typed error (not a broken
// pipe), clean up its temp file, and leave room for small writes.
func TestKrucibleFileWriteDiskFillsMidStream(t *testing.T) {
	eng := newBlockRootEngine(t)
	ke := eng.(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "fillwrite", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), info.ID) })

	const writers, size = 4, 400 << 20 // each fits the free space, 1.6 GB together does not
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = ke.FileWrite(ctx, info.ID, fmt.Sprintf("/workspace/fill%d.bin", i), "0644", size, &patternReader{left: size})
		}()
	}
	wg.Wait()
	noSpace := 0
	for i, err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, agent.ErrNoSpace):
			noSpace++
			t.Logf("writer %d: %v", i, err)
			if _, err := ke.FileStat(ctx, info.ID, fmt.Sprintf("/workspace/fill%d.bin.bhatti-tmp", i)); err == nil {
				t.Errorf("writer %d left its temp file behind", i)
			}
		default:
			t.Errorf("writer %d: %v; want success or agent.ErrNoSpace", i, err)
		}
	}
	if noSpace == 0 {
		t.Fatalf("1.6 GB of uploads all succeeded; the disk should have filled")
	}
	if err := ke.FileWrite(ctx, info.ID, "/workspace/after.txt", "0644", 2, strings.NewReader("ok")); err != nil {
		t.Fatalf("small write after a full disk: %v", err)
	}
}
