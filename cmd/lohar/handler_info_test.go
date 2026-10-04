//go:build linux

package main

import (
	"encoding/json"
	"net"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
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
	for _, f := range []proto.AgentFeature{proto.FeatureNetConfig, proto.FeatureSandboxCA, proto.FeatureRootGrowth, proto.FeaturePipedStderr} {
		if !info.Has(f) {
			t.Errorf("lohar did not advertise %q", f)
		}
	}
}
