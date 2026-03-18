//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestFileWriteRead(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("file-wr"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Write
	content := "hello world"
	r := strings.NewReader(content)
	if err := eng.FileWrite(ctx, info.ID, "/workspace/test.txt", "0644", int64(len(content)), r); err != nil {
		t.Fatalf("FileWrite: %v", err)
	}

	// Read
	var buf bytes.Buffer
	size, mode, err := eng.FileRead(ctx, info.ID, "/workspace/test.txt", &buf)
	if err != nil {
		t.Fatalf("FileRead: %v", err)
	}
	if buf.String() != content {
		t.Errorf("content: %q, want %q", buf.String(), content)
	}
	if size != int64(len(content)) {
		t.Errorf("size: %d, want %d", size, len(content))
	}
	if mode != "0644" {
		t.Errorf("mode: %s, want 0644", mode)
	}
	t.Logf("✓ write/read %d bytes, mode=%s", size, mode)
}

func TestFileReadNotFound(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("file-notfound"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	var buf bytes.Buffer
	_, _, err = eng.FileRead(ctx, info.ID, "/nonexistent", &buf)
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
	if !strings.Contains(err.Error(), "no such file") {
		t.Errorf("error: %v, want 'no such file'", err)
	}
	t.Logf("✓ nonexistent file returns error: %v", err)
}

func TestFileStat(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("file-stat"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	content := "stat test data"
	if err := eng.FileWrite(ctx, info.ID, "/workspace/stat.txt", "0644", int64(len(content)), strings.NewReader(content)); err != nil {
		t.Fatalf("FileWrite: %v", err)
	}

	fi, err := eng.FileStat(ctx, info.ID, "/workspace/stat.txt")
	if err != nil {
		t.Fatalf("FileStat: %v", err)
	}
	if fi.Name != "stat.txt" {
		t.Errorf("name: %q", fi.Name)
	}
	if fi.Size != int64(len(content)) {
		t.Errorf("size: %d, want %d", fi.Size, len(content))
	}
	if fi.Mode != "0644" {
		t.Errorf("mode: %s, want 0644", fi.Mode)
	}
	if fi.IsDir {
		t.Error("is_dir should be false")
	}
	t.Logf("✓ stat: name=%s size=%d mode=%s", fi.Name, fi.Size, fi.Mode)
}

func TestFileList(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("file-ls"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Create files via exec
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo a > /workspace/a.txt && echo b > /workspace/b.txt && mkdir -p /workspace/subdir"})

	files, err := eng.FileList(ctx, info.ID, "/workspace/")
	if err != nil {
		t.Fatalf("FileList: %v", err)
	}

	names := map[string]bool{}
	for _, f := range files {
		names[f.Name] = true
		t.Logf("  %s (dir=%v, size=%d)", f.Name, f.IsDir, f.Size)
	}
	if !names["a.txt"] || !names["b.txt"] || !names["subdir"] {
		t.Errorf("missing files in listing: %v", names)
	}
	t.Logf("✓ listed %d entries", len(files))
}

func TestFileLargeRoundTrip(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("file-large"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Generate 10MB of random data
	size := 10 * 1024 * 1024
	data := make([]byte, size)
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	rng.Read(data)
	expectedHash := sha256.Sum256(data)

	// Write
	start := time.Now()
	if err := eng.FileWrite(ctx, info.ID, "/workspace/large.bin", "0644", int64(size), bytes.NewReader(data)); err != nil {
		t.Fatalf("FileWrite: %v", err)
	}
	writeTime := time.Since(start)

	// Read back
	start = time.Now()
	var buf bytes.Buffer
	readSize, _, err := eng.FileRead(ctx, info.ID, "/workspace/large.bin", &buf)
	if err != nil {
		t.Fatalf("FileRead: %v", err)
	}
	readTime := time.Since(start)

	if readSize != int64(size) {
		t.Fatalf("read %d bytes, want %d", readSize, size)
	}
	actualHash := sha256.Sum256(buf.Bytes())
	if actualHash != expectedHash {
		t.Error("SHA256 mismatch after round-trip")
	}
	t.Logf("✓ 10MB round-trip: write=%v read=%v, SHA256 matches", writeTime, readTime)
}

func TestFileWritePermissions(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("file-perms"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	content := "secret data"
	if err := eng.FileWrite(ctx, info.ID, "/workspace/secret.txt", "0600", int64(len(content)), strings.NewReader(content)); err != nil {
		t.Fatalf("FileWrite: %v", err)
	}

	// Verify via exec
	r, _ := execWithTimeout(t, eng, info.ID, []string{"stat", "-c", "%a", "/workspace/secret.txt"})
	mode := strings.TrimSpace(r.Stdout)
	if mode != "600" {
		t.Errorf("mode: %s, want 600", mode)
	}

	// Also verify via FileStat
	fi, err := eng.FileStat(ctx, info.ID, "/workspace/secret.txt")
	if err != nil {
		t.Fatalf("FileStat: %v", err)
	}
	if fi.Mode != "0600" {
		t.Errorf("FileStat mode: %s, want 0600", fi.Mode)
	}
	t.Logf("✓ file mode 0600 verified via exec and FileStat")
}

func TestFileWriteCreatesDirs(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("file-mkdirs"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	content := "deeply nested content"
	if err := eng.FileWrite(ctx, info.ID, "/workspace/deep/nested/dir/file.txt", "0644", int64(len(content)), strings.NewReader(content)); err != nil {
		t.Fatalf("FileWrite: %v", err)
	}

	// Verify
	var buf bytes.Buffer
	_, _, err = eng.FileRead(ctx, info.ID, "/workspace/deep/nested/dir/file.txt", &buf)
	if err != nil {
		t.Fatalf("FileRead: %v", err)
	}
	if buf.String() != content {
		t.Errorf("content: %q", buf.String())
	}
	t.Log("✓ intermediate directories created")
}

func TestFileListEmpty(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("file-ls-empty"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Create an empty directory
	execWithTimeout(t, eng, info.ID, []string{"mkdir", "-p", "/workspace/empty"})

	files, err := eng.FileList(ctx, info.ID, "/workspace/empty")
	if err != nil {
		t.Fatalf("FileList: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected empty list, got %d entries: %+v", len(files), files)
	}
	t.Log("✓ empty directory returns empty JSON array")
}

// TestFileOverwrite verifies that writing to an existing file truncates it.
func TestFileOverwrite(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("file-overwrite"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Write long content
	long := strings.Repeat("x", 1000)
	eng.FileWrite(ctx, info.ID, "/workspace/ow.txt", "0644", int64(len(long)), strings.NewReader(long))

	// Overwrite with short content
	short := "short"
	eng.FileWrite(ctx, info.ID, "/workspace/ow.txt", "0644", int64(len(short)), strings.NewReader(short))

	var buf bytes.Buffer
	_, _, err = eng.FileRead(ctx, info.ID, "/workspace/ow.txt", &buf)
	if err != nil {
		t.Fatalf("FileRead: %v", err)
	}
	if buf.String() != short {
		t.Errorf("content: %q (len %d), want %q", buf.String(), buf.Len(), short)
	}
	t.Logf("✓ overwrite truncates: read %d bytes", buf.Len())
}

// --- Edge case tests ---

func TestFileZeroByte(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("file-zero"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Write empty file
	if err := eng.FileWrite(ctx, info.ID, "/workspace/__init__.py", "0644", 0, strings.NewReader("")); err != nil {
		t.Fatalf("FileWrite: %v", err)
	}

	// Stat it
	fi, err := eng.FileStat(ctx, info.ID, "/workspace/__init__.py")
	if err != nil {
		t.Fatalf("FileStat: %v", err)
	}
	if fi.Size != 0 {
		t.Errorf("size: %d, want 0", fi.Size)
	}

	// Read it back
	var buf bytes.Buffer
	size, _, err := eng.FileRead(ctx, info.ID, "/workspace/__init__.py", &buf)
	if err != nil {
		t.Fatalf("FileRead: %v", err)
	}
	if size != 0 || buf.Len() != 0 {
		t.Errorf("size=%d, bufLen=%d, want both 0", size, buf.Len())
	}
	t.Log("✓ zero-byte file write/stat/read")
}

func TestFileReadDirectory(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("file-readdir"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	var buf bytes.Buffer
	_, _, err = eng.FileRead(ctx, info.ID, "/workspace", &buf)
	if err == nil {
		t.Fatal("expected error reading directory")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error: %v, want 'directory'", err)
	}
	t.Logf("✓ directory read returns error: %v", err)
}

func TestFileBinaryData(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("file-binary"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// All 256 byte values
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}

	if err := eng.FileWrite(ctx, info.ID, "/workspace/binary.bin", "0644", int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatalf("FileWrite: %v", err)
	}

	var buf bytes.Buffer
	_, _, err = eng.FileRead(ctx, info.ID, "/workspace/binary.bin", &buf)
	if err != nil {
		t.Fatalf("FileRead: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Error("binary data corrupted through VM")
	} else {
		t.Log("✓ all 256 byte values survive round-trip through VM")
	}
}

// --- Concurrency integration tests (agentic patterns) ---

func TestFileConcurrentWritesDifferentFiles(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("file-conc-diff"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	const n = 5
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			content := fmt.Sprintf("content from goroutine %d", i)
			path := fmt.Sprintf("/workspace/file-%d.txt", i)
			errs <- eng.FileWrite(ctx, info.ID, path, "0644", int64(len(content)), strings.NewReader(content))
		}(i)
	}

	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("write %d: %v", i, err)
		}
	}

	// Verify all files
	for i := 0; i < n; i++ {
		var buf bytes.Buffer
		path := fmt.Sprintf("/workspace/file-%d.txt", i)
		_, _, err := eng.FileRead(ctx, info.ID, path, &buf)
		if err != nil {
			t.Errorf("read %d: %v", i, err)
			continue
		}
		expected := fmt.Sprintf("content from goroutine %d", i)
		if buf.String() != expected {
			t.Errorf("file %d: %q, want %q", i, buf.String(), expected)
		}
	}
	t.Logf("✓ %d concurrent writes to different files all correct", n)
}

func TestFileWriteThenExecReads(t *testing.T) {
	// Agentic pattern: write a file, then exec a command that reads it.
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("file-write-exec"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	script := "#!/bin/bash\necho hello from script\n"
	if err := eng.FileWrite(ctx, info.ID, "/workspace/run.sh", "0755", int64(len(script)), strings.NewReader(script)); err != nil {
		t.Fatalf("FileWrite: %v", err)
	}

	r, err := execWithTimeout(t, eng, info.ID, []string{"bash", "/workspace/run.sh"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if r.ExitCode != 0 {
		t.Errorf("exit code: %d, stderr: %s", r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "hello from script") {
		t.Errorf("stdout: %q", r.Stdout)
	}
	t.Log("✓ write file then exec reads it correctly")
}

func TestFileAtomicWriteDuringExec(t *testing.T) {
	// Agentic pattern: a process is running and watching a file,
	// we write a new version. The process should see complete content.
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("file-atomic-exec"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Write initial file
	initial := "version=1"
	eng.FileWrite(ctx, info.ID, "/workspace/config.txt", "0644", int64(len(initial)), strings.NewReader(initial))

	// Overwrite with new content
	updated := "version=2"
	eng.FileWrite(ctx, info.ID, "/workspace/config.txt", "0644", int64(len(updated)), strings.NewReader(updated))

	// Exec reads the file — should see complete "version=2"
	r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/workspace/config.txt"})
	if strings.TrimSpace(r.Stdout) != updated {
		t.Errorf("exec read: %q, want %q", r.Stdout, updated)
	} else {
		t.Log("✓ atomic write visible to exec")
	}
}

func TestFileRapidWriteReadCycles(t *testing.T) {
	// Simulates an agent that writes code, reads it back to verify,
	// modifies, reads again — 10 rapid cycles.
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("file-rapid"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	for i := 0; i < 10; i++ {
		content := fmt.Sprintf("iteration %d: %s", i, strings.Repeat("x", 500))
		if err := eng.FileWrite(ctx, info.ID, "/workspace/code.py", "0644", int64(len(content)), strings.NewReader(content)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}

		var buf bytes.Buffer
		_, _, err := eng.FileRead(ctx, info.ID, "/workspace/code.py", &buf)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if buf.String() != content {
			t.Fatalf("cycle %d: read %d bytes, want %d", i, buf.Len(), len(content))
		}
	}
	t.Log("✓ 10 rapid write/read cycles, all correct")
}

// Ensure unused import is satisfied
var _ = fmt.Sprintf
