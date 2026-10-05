//go:build linux

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"golang.org/x/sys/unix"
)

func TestInfoFrameReportsBakedVersionAndFeatures(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	oldToken := agentToken
	agentToken = ""
	defer func() { agentToken = oldToken }()
	go handleControlConnection(guest)
	if err := proto.WriteFrame(host, proto.INFO_REQ, nil); err != nil {
		t.Fatal(err)
	}
	typ, payload, err := proto.ReadFrame(host)
	if err != nil || typ != proto.INFO_RESP {
		t.Fatalf("info reply type 0x%02x, err %v", typ, err)
	}
	var info proto.AgentInfo
	if err := json.Unmarshal(payload, &info); err != nil || info.Version != version || info.Legacy {
		t.Fatalf("info = %+v, %v; want baked version %q", info, err, version)
	}
	if expected := os.Getenv("LOHAR_EXPECT_VERSION"); expected != "" && info.Version != expected {
		t.Fatalf("info version = %q; want ldflag-stamped %q", info.Version, expected)
	}
	for _, f := range []proto.AgentFeature{proto.FeatureNetConfigMAC, proto.FeatureSandboxCA, proto.FeatureRootGrowth, proto.FeaturePipedStderr, proto.FeatureExecSync, proto.FeatureFSFreeze} {
		if !info.Has(f) {
			t.Errorf("lohar did not advertise %q", f)
		}
	}
}

func TestNetConfigMACLinkPayload(t *testing.T) {
	mac := net.HardwareAddr{0x52, 0x54, 0, 0, 0, 3}
	payload := macLinkPayload(42, mac)
	off := unix.SizeofIfInfomsg
	if payload[0] != unix.AF_UNSPEC || binary.NativeEndian.Uint32(payload[4:]) != 42 {
		t.Fatalf("netlink interface header does not select eth0 index 42: %x", payload[:off])
	}
	if size, kind := binary.NativeEndian.Uint16(payload[off:]), binary.NativeEndian.Uint16(payload[off+2:]); size != unix.SizeofRtAttr+6 || kind != unix.IFLA_ADDRESS {
		t.Fatalf("MAC rtattr type=%d len=%d, want IFLA_ADDRESS/10", kind, size)
	}
	if !bytes.Equal(payload[off+unix.SizeofRtAttr:off+unix.SizeofRtAttr+6], mac) {
		t.Fatalf("MAC rtattr payload = %x, want %x", payload[off:], mac)
	}
}

func TestFilesystemQuiescenceValidatesMountAndAcknowledgesThaw(t *testing.T) {
	mounts := "24 23 0:19 / /mnt/data\\040one rw,relatime - ext4 /dev/vdb rw\n"
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/mnt/data one", true},
		{"/mnt/data", false},
		{"/mnt/data one/child", false},
	} {
		got, err := exactMountpoint(strings.NewReader(mounts), tc.path)
		if err != nil || got != tc.want {
			t.Fatalf("exactMountpoint(%q) = %t, %v; want %t", tc.path, got, err, tc.want)
		}
	}

	for _, tc := range []struct {
		name      string
		operation byte
		mount     string
		ioctlErr  error
		want      byte
		called    bool
	}{
		{"freeze", proto.FREEZE_REQ, "/", nil, proto.FREEZE_ACK, true},
		{"trailing slash", proto.FREEZE_REQ, "/proc/", nil, proto.FREEZE_ACK, true},
		{"already thawed", proto.THAW_REQ, "/", unix.EINVAL, proto.THAW_ACK, true},
		{"bad path", proto.FREEZE_REQ, "/proc/self/mountinfo", nil, proto.ERROR, false},
		{"traversal", proto.FREEZE_REQ, "/proc/../", nil, proto.ERROR, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, guest := net.Pipe()
			defer host.Close()
			called := make(chan uint, 1)
			go func() {
				defer guest.Close()
				payload, _ := json.Marshal(proto.FSFreezeRequest{Mount: tc.mount})
				handleFSFreezeWithIoctl(guest, tc.operation, payload, func(_ int, request uint, _ int) error {
					called <- request
					return tc.ioctlErr
				})
			}()
			typ, payload, err := proto.ReadFrame(host)
			if err != nil || typ != tc.want {
				t.Fatalf("quiescence reply = 0x%02x %q, %v; want 0x%02x", typ, payload, err, tc.want)
			}
			if tc.called {
				request := <-called
				want := uint(fifreezeIoctl)
				if tc.operation == proto.THAW_REQ {
					want = fithawIoctl
				}
				if request != want || len(payload) != 0 {
					t.Fatalf("ioctl=0x%x reply=%q; want 0x%x empty ACK", request, payload, want)
				}
			} else if len(called) != 0 {
				t.Fatal("unsafe path reached filesystem ioctl")
			}
		})
	}
}

func TestThawWaitsForInFlightFreeze(t *testing.T) {
	freezeHost, freezeGuest := net.Pipe()
	defer freezeHost.Close()
	thawHost, thawGuest := net.Pipe()
	defer thawHost.Close()
	freezeHost.SetReadDeadline(time.Now().Add(time.Second))
	thawHost.SetReadDeadline(time.Now().Add(time.Second))

	payload, _ := json.Marshal(proto.FSFreezeRequest{Mount: "/"})
	entered, release := make(chan struct{}), make(chan struct{})
	go func() {
		defer freezeGuest.Close()
		handleFSFreezeWithIoctl(freezeGuest, proto.FREEZE_REQ, payload, func(_ int, operation uint, _ int) error {
			if operation == fithawIoctl {
				return unix.EINVAL
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	thawIoctl := make(chan struct{})
	go func() {
		defer thawGuest.Close()
		handleFSFreezeWithIoctl(thawGuest, proto.THAW_REQ, payload, func(_ int, _ uint, _ int) error {
			close(thawIoctl)
			return unix.EINVAL
		})
	}()
	select {
	case <-thawIoctl:
		close(release)
		t.Fatal("THAW ran before in-flight FREEZE finished; cleanup can falsely succeed")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	for _, reply := range []struct {
		conn net.Conn
		want byte
	}{
		{freezeHost, proto.FREEZE_ACK},
		{thawHost, proto.THAW_ACK},
	} {
		typ, body, err := proto.ReadFrame(reply.conn)
		if err != nil || typ != reply.want || len(body) != 0 {
			t.Fatalf("ordered quiescence reply = 0x%02x %q, %v; want empty 0x%02x", typ, body, err, reply.want)
		}
	}
}

func TestFreezeLeaseThawsOnHostDisconnectEvenAfterLateFreeze(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		name := "after ack"
		if delayed {
			name = "before late ack"
		}
		t.Run(name, func(t *testing.T) {
			host, guest := net.Pipe()
			defer host.Close()
			payload, _ := json.Marshal(proto.FSFreezeRequest{Mount: "/proc/"})
			entered := make(chan struct{})
			release := make(chan struct{})
			done := make(chan struct{})
			ioctls := make(chan uint, 2)
			go func() {
				defer close(done)
				defer guest.Close()
				handleFSFreezeWithIoctl(guest, proto.FREEZE_REQ, payload, func(_ int, operation uint, _ int) error {
					ioctls <- operation
					if operation == fifreezeIoctl {
						close(entered)
						if delayed {
							<-release
						}
					}
					return nil
				})
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("guest never entered FIFREEZE")
			}
			if !delayed {
				typ, _, err := proto.ReadFrame(host)
				if err != nil || typ != proto.FREEZE_ACK {
					t.Fatalf("freeze ack = 0x%02x, %v", typ, err)
				}
			}
			host.Close()
			close(release)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("guest did not auto-thaw after host EOF")
			}
			if len(ioctls) != 2 {
				t.Fatalf("guest issued %d filesystem ioctls after host EOF; want FREEZE then THAW", len(ioctls))
			}
			first, second := <-ioctls, <-ioctls
			if first != fifreezeIoctl || second != fithawIoctl || len(ioctls) != 0 {
				t.Fatalf("ioctls on host death = 0x%x 0x%x, additional=%d; want freeze then thaw", first, second, len(ioctls))
			}
		})
	}
}

func TestFreezeLeaseExplicitThawOnSameConnection(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	payload, _ := json.Marshal(proto.FSFreezeRequest{Mount: "/proc/"})
	ioctls := make(chan uint, 3)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer guest.Close()
		handleFSFreezeWithIoctl(guest, proto.FREEZE_REQ, payload, func(_ int, operation uint, _ int) error {
			ioctls <- operation
			return nil
		})
	}()
	host.SetDeadline(time.Now().Add(time.Second))
	if typ, _, err := proto.ReadFrame(host); err != nil || typ != proto.FREEZE_ACK {
		t.Fatalf("freeze lease ack = 0x%02x, %v", typ, err)
	}
	if err := proto.WriteFrame(host, proto.THAW_REQ, payload); err != nil {
		t.Fatal(err)
	}
	if typ, body, err := proto.ReadFrame(host); err != nil || typ != proto.THAW_ACK || len(body) != 0 {
		t.Fatalf("explicit thaw ack = 0x%02x %q, %v", typ, body, err)
	}
	<-done
	first, second := <-ioctls, <-ioctls
	if first != fifreezeIoctl || second != fithawIoctl || len(ioctls) != 0 {
		t.Fatalf("ioctls on explicit thaw = 0x%x 0x%x, additional=%d", first, second, len(ioctls))
	}
}
