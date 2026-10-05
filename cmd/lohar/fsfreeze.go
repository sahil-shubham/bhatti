//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"golang.org/x/sys/unix"
)

// Serialize FREEZE and THAW ioctls so cleanup cannot observe EINVAL while
// an earlier FREEZE is still in progress and then return before it completes.
var fsQuiesceMu sync.Mutex

// linux/fs.h defines FIFREEZE/FITHAW as _IOWR('X', 119/120, int).
// x/sys/unix does not expose these filesystem ioctl request numbers.
const (
	fifreezeIoctl = 0xC0045877
	fithawIoctl   = 0xC0045878
)

func handleFSFreeze(conn net.Conn, operation byte, payload []byte) {
	handleFSFreezeWithIoctl(conn, operation, payload, unix.IoctlSetInt)
}

func handleFSFreezeWithIoctl(conn net.Conn, operation byte, payload []byte, ioctl func(int, uint, int) error) {
	var req proto.FSFreezeRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("bad filesystem quiescence request: %v", err)))
		return
	}
	if err := quiesceMount(req.Mount, operation, ioctl); err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(err.Error()))
		return
	}
	if operation == proto.THAW_REQ {
		proto.WriteFrame(conn, proto.THAW_ACK, nil)
		return
	}

	// A frozen filesystem belongs to this connection, not to the lifetime of
	// a host daemon. If the host dies or a FREEZE times out while the ioctl is
	// pending, EOF (or a failed ACK write) releases the filesystem automatically.
	frozen := true
	defer func() {
		if frozen {
			if err := quiesceMount(req.Mount, proto.THAW_REQ, ioctl); err != nil {
				logf("auto-thaw %q after control disconnect: %v", req.Mount, err)
			}
		}
	}()
	if err := proto.WriteFrame(conn, proto.FREEZE_ACK, nil); err != nil {
		return
	}
	msgType, body, err := proto.ReadFrame(conn)
	if err != nil {
		return
	}
	if msgType != proto.THAW_REQ {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("expected THAW_REQ, got 0x%02x", msgType)))
		return
	}
	var thaw proto.FSFreezeRequest
	if err := json.Unmarshal(body, &thaw); err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("bad thaw request: %v", err)))
		return
	}
	original, err := canonicalMount(req.Mount)
	if err != nil {
		return
	}
	matching, err := canonicalMount(thaw.Mount)
	if err != nil || original != matching {
		proto.WriteFrame(conn, proto.ERROR, []byte("thaw mount does not match frozen mount"))
		return
	}
	if err := quiesceMount(req.Mount, proto.THAW_REQ, ioctl); err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(err.Error()))
		return
	}
	frozen = false
	proto.WriteFrame(conn, proto.THAW_ACK, nil)
}

func canonicalMount(mount string) (string, error) {
	if !filepath.IsAbs(mount) {
		return "", fmt.Errorf("mount must be absolute: %q", mount)
	}
	clean := filepath.Clean(mount)
	// Accept redundant trailing slashes without allowing traversal or an
	// ordinary path inside a mount to select its mounted parent.
	if mount != clean && strings.TrimRight(mount, "/") != clean &&
		!(clean == "/" && strings.Trim(mount, "/") == "") {
		return "", fmt.Errorf("mount must be a clean path: %q", mount)
	}
	return clean, nil
}

func quiesceMount(mount string, operation byte, ioctl func(int, uint, int) error) error {
	clean, err := canonicalMount(mount)
	if err != nil {
		return err
	}
	mount = clean
	fsQuiesceMu.Lock()
	defer fsQuiesceMu.Unlock()
	mountinfo, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("read mountinfo: %w", err)
	}
	mounted, err := exactMountpoint(mountinfo, mount)
	mountinfo.Close()
	if err != nil {
		return fmt.Errorf("read mountinfo: %w", err)
	}
	if !mounted {
		return fmt.Errorf("not a guest mountpoint: %q", mount)
	}

	// Operate on the directory itself, never on a symlink or a file supplied
	// by an API caller. In particular an ordinary path inside a mount must not
	// be able to freeze its containing filesystem.
	fd, err := unix.Open(mount, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open mountpoint %q: %w", mount, err)
	}
	defer unix.Close(fd)

	switch operation {
	case proto.FREEZE_REQ:
		err = ioctl(fd, fifreezeIoctl, 0)
	case proto.THAW_REQ:
		err = ioctl(fd, fithawIoctl, 0)
		// A FREEZE reply can be lost or time out before ioctl completes. Cleanup
		// always attempts THAW, including when no freeze actually happened.
		if errors.Is(err, unix.EINVAL) {
			return nil
		}
	default:
		return fmt.Errorf("unsupported filesystem quiescence operation 0x%02x", operation)
	}
	if err != nil {
		return fmt.Errorf("filesystem quiescence ioctl on %q: %w", mount, err)
	}
	return nil
}

// mountinfo uses octal escapes for spaces, backslashes and newlines. Compare
// decoded, exact mountpoints rather than prefixes (e.g. /data vs /data-other).
func exactMountpoint(r io.Reader, mount string) (bool, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			return false, fmt.Errorf("malformed mountinfo entry")
		}
		path, err := strconv.Unquote(`"` + fields[4] + `"`)
		if err != nil {
			return false, fmt.Errorf("decode mountinfo path: %w", err)
		}
		if path == mount {
			return true, nil
		}
	}
	return false, scanner.Err()
}
