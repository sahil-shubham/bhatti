package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// fakeAgent answers frames the way a lohar built before INFO_REQ did. In
// particular, it rejects unknown frame types instead of dropping the socket.
func fakeAgent(t *testing.T, reply func(byte, []byte) (byte, []byte)) *AgentClient {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bh-info-") // macOS sockaddr_un cannot hold testing.T.TempDir's path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(2 * time.Second))
				typ, payload, err := proto.ReadFrame(conn)
				if err != nil {
					return
				}
				resp, data := reply(typ, payload)
				proto.WriteFrame(conn, resp, data)
			}()
		}
	}()
	return NewTestClient(path, path)
}

func oldAgentReply(typ byte, _ []byte) (byte, []byte) {
	return proto.ERROR, []byte(fmt.Sprintf("unexpected frame type 0x%02x", typ))
}

func TestAgentInfoLegacyOnlyOnExactUnexpectedFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	old := fakeAgent(t, oldAgentReply)
	info, err := old.Info(ctx)
	if err != nil || !info.Legacy || info.Version != "" || len(info.Features) != 0 {
		t.Fatalf("old lohar info = %+v, %v; want legacy with no features", info, err)
	}
	for _, reply := range []func(byte, []byte) (byte, []byte){
		func(byte, []byte) (byte, []byte) { return proto.ERROR, []byte("auth required") },
		func(byte, []byte) (byte, []byte) { return proto.ERROR, []byte("unexpected frame type 0x14") },
		func(byte, []byte) (byte, []byte) { return proto.INFO_RESP, []byte(`{"version":"","features":[]}`) },
		func(byte, []byte) (byte, []byte) { return proto.ACTIVITY_RESP, nil },
	} {
		guest := fakeAgent(t, reply)
		info, err := guest.Info(ctx)
		if err == nil || info.Legacy {
			t.Fatalf("non-legacy failure labeled legacy: %+v, %v", info, err)
		}
	}
}

func TestAgentInfoReportsVersionAndFeatures(t *testing.T) {
	guest := fakeAgent(t, func(typ byte, _ []byte) (byte, []byte) {
		if typ != proto.INFO_REQ {
			return oldAgentReply(typ, nil)
		}
		return proto.INFO_RESP, []byte(`{"version":"v2.4.0","features":["net_config","sandbox_ca","root_growth","piped_stderr"]}`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, err := guest.Info(ctx)
	if err != nil || info.Legacy || info.Version != "v2.4.0" {
		t.Fatalf("info = %+v, %v", info, err)
	}
	for _, feature := range []proto.AgentFeature{proto.FeatureNetConfig, proto.FeatureSandboxCA, proto.FeatureRootGrowth, proto.FeaturePipedStderr} {
		if !info.Has(feature) {
			t.Fatalf("agent info missing %s: %+v", feature, info)
		}
	}
}

func TestOldAgentNetConfigMapsOnlyUnknownFrameToOutdated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	old := fakeAgent(t, oldAgentReply)
	if err := old.NetConfig(ctx, "100.64.1.2/24", "100.64.1.1"); !errors.Is(err, engine.ErrGuestAgentOutdated) || !strings.Contains(err.Error(), "recreate it") {
		t.Fatalf("old NET_CONFIG error = %v; want actionable outdated conflict", err)
	}
	broken := fakeAgent(t, func(byte, []byte) (byte, []byte) {
		return proto.ERROR, []byte("reconfigure eth0: permission denied")
	})
	if err := broken.NetConfig(ctx, "100.64.1.2/24", "100.64.1.1"); err == nil || errors.Is(err, engine.ErrGuestAgentOutdated) {
		t.Fatalf("real network failure mislabeled outdated: %v", err)
	}
}

func TestReseedSendsEntropyAndRequiresGuestAcknowledgement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var seed [proto.ReseedBytes]byte
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	received := make(chan []byte, 1)
	guest := fakeAgent(t, func(typ byte, payload []byte) (byte, []byte) {
		if typ != proto.RESEED {
			return proto.ERROR, []byte("unexpected frame")
		}
		received <- bytes.Clone(payload)
		return proto.RESEED, nil
	})
	if err := guest.Reseed(ctx, seed); err != nil {
		t.Fatal(err)
	}
	if got := <-received; !bytes.Equal(got, seed[:]) {
		t.Fatalf("reseed sent %x, want %x", got, seed)
	}
	for _, tc := range []struct {
		name    string
		typ     byte
		payload []byte
	}{
		{"guest error", proto.ERROR, []byte("RNDADDENTROPY: permission denied")},
		{"wrong ack", proto.ACTIVITY_RESP, nil},
		{"nonempty ack", proto.RESEED, []byte("ignored")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broken := fakeAgent(t, func(byte, []byte) (byte, []byte) { return tc.typ, tc.payload })
			if err := broken.Reseed(ctx, seed); err == nil {
				t.Fatal("guest reseed without a successful acknowledgement was accepted")
			}
		})
	}
}
