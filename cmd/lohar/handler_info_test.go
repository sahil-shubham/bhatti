//go:build linux

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"testing"

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
	for _, f := range []proto.AgentFeature{proto.FeatureNetConfigMAC, proto.FeatureSandboxCA, proto.FeatureRootGrowth, proto.FeaturePipedStderr} {
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
