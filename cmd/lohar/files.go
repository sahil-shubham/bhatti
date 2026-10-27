//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

func handleFileRead(conn net.Conn, payload []byte) {
	var req struct {
		Path     string `json:"path"`
		Offset   int    `json:"offset,omitempty"`    // 1-indexed line, default 0 (start)
		Limit    int    `json:"limit,omitempty"`     // max lines, 0 = unlimited
		MaxBytes int    `json:"max_bytes,omitempty"` // max bytes, 0 = unlimited
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("bad request: %v", err)))
		return
	}

	f, err := os.Open(req.Path)
	if err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(err.Error()))
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(err.Error()))
		return
	}

	// Reject directories — use ls=true for directory listings
	if info.IsDir() {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("%s is a directory", req.Path)))
		return
	}

	// Reject non-regular files (devices, pipes, sockets)
	if !info.Mode().IsRegular() {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("%s is not a regular file", req.Path)))
		return
	}

	proto.SendJSON(conn, proto.FILE_READ_RESP, map[string]any{
		"size": info.Size(),
		"mode": fmt.Sprintf("%04o", info.Mode().Perm()),
	})

	// Truncated read: line-based with offset/limit/max_bytes
	if req.Offset > 0 || req.Limit > 0 || req.MaxBytes > 0 {
		handleFileReadTruncated(conn, f, req.Offset, req.Limit, req.MaxBytes)
		return
	}

	// Full read (existing path, backward compatible)
	buf := make([]byte, 32768)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			proto.WriteFrame(conn, proto.STDOUT, buf[:n])
		}
		if err != nil {
			break
		}
	}
	exit := proto.ExitPayload(0)
	proto.WriteFrame(conn, proto.EXIT, exit[:])
}

// handleFileReadTruncated reads a file line-by-line, applying offset (1-indexed),
// limit (max lines), and max_bytes (byte budget) constraints. Whichever limit
// is hit first stops the read.
func handleFileReadTruncated(conn net.Conn, f *os.File, offset, limit, maxBytes int) {
	if offset < 1 {
		offset = 1
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024) // 256KB max line

	lineNum := 0
	sentLines := 0
	sentBytes := 0

	for scanner.Scan() {
		lineNum++
		if lineNum < offset {
			continue
		}
		if limit > 0 && sentLines >= limit {
			break
		}

		line := scanner.Bytes()
		// Append newline (scanner strips it)
		out := make([]byte, len(line)+1)
		copy(out, line)
		out[len(line)] = '\n'

		// Check byte budget before sending
		if maxBytes > 0 && sentBytes+len(out) > maxBytes {
			remaining := maxBytes - sentBytes
			if remaining > 0 {
				proto.WriteFrame(conn, proto.STDOUT, out[:remaining])
			}
			break
		}

		proto.WriteFrame(conn, proto.STDOUT, out)
		sentLines++
		sentBytes += len(out)
	}

	exit := proto.ExitPayload(0)
	proto.WriteFrame(conn, proto.EXIT, exit[:])
}

func handleFileWrite(conn net.Conn, payload []byte) {
	var req struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
		Size int64  `json:"size"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("bad request: %v", err)))
		return
	}

	// Reject negative sizes (Content-Length -1 = unknown)
	if req.Size < 0 {
		proto.WriteFrame(conn, proto.ERROR, []byte("file size must be >= 0"))
		return
	}

	mode, _ := strconv.ParseUint(req.Mode, 8, 32)
	if mode == 0 {
		mode = 0644
	}

	os.MkdirAll(filepath.Dir(req.Path), 0755)

	// Refuse up front, before the host streams the body, so it gets a clear
	// reply instead of a broken pipe halfway through a multi-GB upload.
	if err := checkWriteSpace(filepath.Dir(req.Path), req.Size); err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(err.Error()))
		return
	}

	// Atomic write: write to a temp file, then rename.
	// This ensures readers never see partial content.
	tmpPath := req.Path + ".bhatti-tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(mode))
	if err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(err.Error()))
		return
	}

	var written int64
	var writeErr error
	for written < req.Size {
		msgType, data, err := proto.ReadFrame(conn)
		if err != nil {
			writeErr = fmt.Errorf("connection lost after %d/%d bytes", written, req.Size)
			break
		}
		if msgType == proto.STDIN {
			n, err := f.Write(data)
			written += int64(n)
			if err != nil {
				writeErr = describeWriteErr(err, written, req.Size)
				break
			}
		}
	}

	// Fsync before rename to ensure data is on disk. Only ENOSPC/EDQUOT is
	// surfaced: delayed allocation can report them here instead of on Write.
	if err := f.Sync(); writeErr == nil && isNoSpace(err) {
		writeErr = describeWriteErr(err, written, req.Size)
	}
	f.Close()

	if writeErr != nil {
		// Clean up partial temp file on error
		os.Remove(tmpPath)
		proto.WriteFrame(conn, proto.ERROR, []byte(writeErr.Error()))
		return
	}

	// Atomic rename: readers see old file or new file, never partial
	if err := os.Rename(tmpPath, req.Path); err != nil {
		os.Remove(tmpPath)
		proto.WriteFrame(conn, proto.ERROR, []byte(err.Error()))
		return
	}

	// chown to lohar user (uid 1000)
	os.Chown(req.Path, 1000, 1000)

	proto.SendJSON(conn, proto.FILE_WRITE_RESP, map[string]string{"status": "ok"})
}

// statfsFn is the free-space probe; tests replace it to simulate a full disk.
var statfsFn = syscall.Statfs

// writeSpaceReserve is kept free on bulk writes so an upload cannot take the
// guest filesystem to zero and wedge the sandbox (lohar needs room for logs,
// temp files and state). Writes no bigger than the reserve are exempt.
const writeSpaceReserve = 16 << 20

// checkWriteSpace fails with a "no space:" error when size bytes (plus the
// reserve for bulk writes) do not fit on the filesystem holding dir. A failed
// probe is not an error: the ENOSPC check during the write still applies.
func checkWriteSpace(dir string, size int64) error {
	var st syscall.Statfs_t
	if err := statfsFn(dir, &st); err != nil {
		return nil
	}
	avail := uint64(st.Bavail) * uint64(st.Bsize)
	need := uint64(size)
	if need > writeSpaceReserve {
		need += writeSpaceReserve
	}
	if need > avail {
		return fmt.Errorf("%s need %d bytes (%s), %d bytes (%s) available", proto.ErrPrefixNoSpace,
			need, humanBytes(need), avail, humanBytes(avail))
	}
	return nil
}

func isNoSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

// describeWriteErr tags ENOSPC mid-stream with the stable prefix the host
// maps to a typed error; other errors pass through unchanged.
func describeWriteErr(err error, written, size int64) error {
	if isNoSpace(err) {
		return fmt.Errorf("%s disk full after %d of %d bytes", proto.ErrPrefixNoSpace, written, size)
	}
	return err
}

func handleFileStat(conn net.Conn, payload []byte) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("bad request: %v", err)))
		return
	}

	info, err := os.Stat(req.Path)
	if err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(err.Error()))
		return
	}
	proto.SendJSON(conn, proto.FILE_STAT_RESP, proto.FileInfo{
		Name:  info.Name(),
		Size:  info.Size(),
		Mode:  fmt.Sprintf("%04o", info.Mode().Perm()),
		IsDir: info.IsDir(),
		Mtime: info.ModTime().Unix(),
	})
}

// maxListEntries caps directory listings to avoid exceeding MaxFrameSize.
const maxListEntries = 10000

func handleFileList(conn net.Conn, payload []byte) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("bad request: %v", err)))
		return
	}

	// Lstat the path first to ensure it's a directory
	info, err := os.Stat(req.Path)
	if err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(err.Error()))
		return
	}
	if !info.IsDir() {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("%s is not a directory", req.Path)))
		return
	}

	// Use Readdirnames + Lstat instead of ReadDir so we can cap the count
	// and avoid loading all DirEntry metadata upfront for huge dirs.
	dir, err := os.Open(req.Path)
	if err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(err.Error()))
		return
	}
	defer dir.Close()

	names, err := dir.Readdirnames(-1)
	if err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(err.Error()))
		return
	}

	// Cap entries to prevent exceeding MaxFrameSize
	truncated := false
	if len(names) > maxListEntries {
		names = names[:maxListEntries]
		truncated = true
	}

	files := make([]proto.FileInfo, 0, len(names))
	for _, name := range names {
		fi, err := os.Lstat(filepath.Join(req.Path, name))
		if err != nil {
			continue // file may have been deleted between readdir and stat
		}
		files = append(files, proto.FileInfo{
			Name:  fi.Name(),
			Size:  fi.Size(),
			Mode:  fmt.Sprintf("%04o", fi.Mode().Perm()),
			IsDir: fi.IsDir(),
			Mtime: fi.ModTime().Unix(),
		})
	}

	if truncated {
		// Signal truncation by adding a sentinel entry
		files = append(files, proto.FileInfo{
			Name:  fmt.Sprintf("... truncated at %d entries (%d total)", maxListEntries, len(names)+1),
			IsDir: false,
		})
	}

	proto.SendJSON(conn, proto.FILE_LS_RESP, files)
}
