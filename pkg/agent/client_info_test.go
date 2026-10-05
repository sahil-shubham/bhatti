package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
				for {
					typ, payload, err := proto.ReadFrame(conn)
					if err != nil {
						return
					}
					resp, data := reply(typ, payload)
					if err := proto.WriteFrame(conn, resp, data); err != nil {
						return
					}
				}
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
		return proto.INFO_RESP, []byte(`{"version":"v2.5.0","features":["net_config_mac","sandbox_ca","root_growth","piped_stderr"]}`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, err := guest.Info(ctx)
	if err != nil || info.Legacy || info.Version != "v2.5.0" {
		t.Fatalf("info = %+v, %v", info, err)
	}
	for _, feature := range []proto.AgentFeature{proto.FeatureNetConfigMAC, proto.FeatureSandboxCA, proto.FeatureRootGrowth, proto.FeaturePipedStderr} {
		if !info.Has(feature) {
			t.Fatalf("agent info missing %s: %+v", feature, info)
		}
	}
}

func TestOldAgentNetConfigMapsOnlyUnknownFrameToOutdated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	old := fakeAgent(t, oldAgentReply)
	if err := old.NetConfig(ctx, "100.64.1.2/24", "100.64.1.1", "52:54:00:00:00:02"); !errors.Is(err, engine.ErrGuestAgentOutdated) || !strings.Contains(err.Error(), "recreate it") {
		t.Fatalf("old NET_CONFIG error = %v; want actionable outdated conflict", err)
	}
	broken := fakeAgent(t, func(byte, []byte) (byte, []byte) {
		return proto.ERROR, []byte("reconfigure eth0: permission denied")
	})
	if err := broken.NetConfig(ctx, "100.64.1.2/24", "100.64.1.1", "52:54:00:00:00:02"); err == nil || errors.Is(err, engine.ErrGuestAgentOutdated) {
		t.Fatalf("real network failure mislabeled outdated: %v", err)
	}
}

func TestNetConfigSendsFreshMACWithIP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	received := make(chan []byte, 1)
	guest := fakeAgent(t, func(typ byte, payload []byte) (byte, []byte) {
		if typ != proto.NET_CONFIG {
			return proto.ERROR, []byte("unexpected frame")
		}
		received <- bytes.Clone(payload)
		return proto.NET_CONFIG, nil
	})
	if err := guest.NetConfig(ctx, "100.64.1.3/24", "100.64.1.1", "52:54:00:00:00:03"); err != nil {
		t.Fatal(err)
	}
	var req struct {
		IPCIDR  string `json:"ip_cidr"`
		Gateway string `json:"gateway"`
		MAC     string `json:"mac"`
	}
	if err := json.Unmarshal(<-received, &req); err != nil {
		t.Fatal(err)
	}
	if req.IPCIDR != "100.64.1.3/24" || req.Gateway != "100.64.1.1" || req.MAC != "52:54:00:00:00:03" {
		t.Fatalf("fork net config = %+v, missing allocated MAC", req)
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

func TestExecSyncFramesAreOptInAndRequireAdvertisedCapability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	received := make(chan struct {
		typ     byte
		payload []byte
	}, 8)
	guest := fakeAgent(t, func(typ byte, payload []byte) (byte, []byte) {
		received <- struct {
			typ     byte
			payload []byte
		}{typ, bytes.Clone(payload)}
		switch typ {
		case proto.INFO_REQ:
			return proto.INFO_RESP, []byte(`{"version":"v2.5.1","features":["exec_sync"]}`)
		case proto.EXEC_REQ:
			exit := proto.ExitPayload(0)
			return proto.EXIT, exit[:]
		default:
			return oldAgentReply(typ, payload)
		}
	})
	if _, err := guest.Exec(ctx, []string{"true"}, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := guest.ExecWithSync(ctx, []string{"true"}, nil, "", true); err != nil {
		t.Fatal(err)
	}
	if first, second, third := <-received, <-received, <-received; first.typ != proto.EXEC_REQ ||
		bytes.Contains(first.payload, []byte(`"sync"`)) || second.typ != proto.INFO_REQ ||
		third.typ != proto.EXEC_REQ || !bytes.Contains(third.payload, []byte(`"sync":true`)) {
		t.Fatalf("default / probe / sync frames = %d %s; %d %s; %d %s",
			first.typ, first.payload, second.typ, second.payload, third.typ, third.payload)
	}

	for _, tc := range []struct {
		name  string
		reply func(byte, []byte) (byte, []byte)
		want  error
	}{
		{"legacy", oldAgentReply, engine.ErrGuestAgentOutdated},
		{"prior INFO", func(typ byte, payload []byte) (byte, []byte) {
			if typ == proto.INFO_REQ {
				return proto.INFO_RESP, []byte(`{"version":"v2.5.0","features":["reseed_crng"]}`)
			}
			return oldAgentReply(typ, payload)
		}, engine.ErrGuestAgentOutdated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frames := make(chan byte, 2)
			old := fakeAgent(t, func(typ byte, payload []byte) (byte, []byte) {
				frames <- typ
				return tc.reply(typ, payload)
			})
			_, err := old.ExecWithSync(ctx, []string{"true"}, nil, "", true)
			if !errors.Is(err, tc.want) {
				t.Fatalf("sync on old lohar = %v, want %v", err, tc.want)
			}
			if typ := <-frames; typ != proto.INFO_REQ || len(frames) != 0 {
				t.Fatalf("old lohar received frames %d, additional=%d; must never send sync field", typ, len(frames))
			}
		})
	}
	brokenFrames := make(chan byte, 2)
	broken := fakeAgent(t, func(typ byte, _ []byte) (byte, []byte) {
		brokenFrames <- typ
		return proto.ERROR, []byte("auth required")
	})
	if _, err := broken.ExecWithSync(ctx, []string{"true"}, nil, "", true); err == nil || errors.Is(err, engine.ErrGuestAgentOutdated) {
		t.Fatalf("INFO failure incorrectly treated as old guest: %v", err)
	}
	if typ := <-brokenFrames; typ != proto.INFO_REQ || len(brokenFrames) != 0 {
		t.Fatalf("INFO failure sent exec request: first=0x%02x, additional=%d", typ, len(brokenFrames))
	}
}

func TestFSFreezeRequiresFeatureAndEmptyAcknowledgement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	received := make(chan struct {
		typ     byte
		payload []byte
	}, 3)
	guest := fakeAgent(t, func(typ byte, payload []byte) (byte, []byte) {
		if typ == proto.INFO_REQ {
			return proto.INFO_RESP, []byte(`{"version":"v2.5.1","features":["fs_freeze"]}`)
		}
		received <- struct {
			typ     byte
			payload []byte
		}{typ, bytes.Clone(payload)}
		switch typ {
		case proto.FREEZE_REQ:
			return proto.FREEZE_ACK, nil
		case proto.THAW_REQ:
			return proto.THAW_ACK, nil
		default:
			return oldAgentReply(typ, payload)
		}
	})
	if err := guest.Freeze(ctx, "/mnt/data"); err != nil {
		t.Fatal(err)
	}
	if err := guest.Thaw(ctx, "/mnt/data"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []byte{proto.FREEZE_REQ, proto.THAW_REQ} {
		frame := <-received
		if frame.typ != want || string(frame.payload) != `{"mount":"/mnt/data"}` {
			t.Fatalf("quiesce frame = 0x%02x %s, want 0x%02x and mount", frame.typ, frame.payload, want)
		}
	}
	for _, reply := range []struct {
		typ      byte
		payload  []byte
		rejected bool
	}{
		{proto.ERROR, []byte("FIFREEZE: device or resource busy"), true},
		{proto.FREEZE_ACK, []byte("not empty"), false},
		{proto.THAW_ACK, nil, false},
	} {
		broken := fakeAgent(t, func(typ byte, _ []byte) (byte, []byte) {
			if typ == proto.INFO_REQ {
				return proto.INFO_RESP, []byte(`{"version":"v2.5.1","features":["fs_freeze"]}`)
			}
			return reply.typ, reply.payload
		})
		err := broken.Freeze(ctx, "/mnt/data")
		if err == nil || errors.Is(err, ErrFSFreezeRejected) != reply.rejected {
			t.Fatalf("freeze reply 0x%02x %q => %v, want rejected=%v", reply.typ, reply.payload, err, reply.rejected)
		}
		if reply.rejected && !strings.Contains(err.Error(), "resource busy") {
			t.Fatalf("freeze rejection lost guest EBUSY reason: %v", err)
		}
	}
	oldFrames := make(chan byte, 2)
	old := fakeAgent(t, func(typ byte, payload []byte) (byte, []byte) {
		oldFrames <- typ
		return oldAgentReply(typ, payload)
	})
	if err := old.Freeze(ctx, "/mnt/data"); !errors.Is(err, engine.ErrGuestAgentOutdated) || !errors.Is(err, ErrFSFreezeRejected) {
		t.Fatalf("old lohar freeze = %v; want definitive pre-send rejection", err)
	}
	if typ := <-oldFrames; typ != proto.INFO_REQ || len(oldFrames) != 0 {
		t.Fatalf("old lohar received frames %d, additional=%d; must never send FREEZE_REQ", typ, len(oldFrames))
	}
	probeFrames := make(chan byte, 2)
	probeFailed := fakeAgent(t, func(typ byte, _ []byte) (byte, []byte) {
		probeFrames <- typ
		return proto.ERROR, []byte("INFO unavailable")
	})
	if err := probeFailed.Freeze(ctx, "/mnt/data"); !errors.Is(err, ErrFSFreezeRejected) {
		t.Fatalf("failed pre-send probe = %v; want definitive rejection", err)
	}
	if first := <-probeFrames; first != proto.INFO_REQ || len(probeFrames) != 0 {
		t.Fatalf("failed probe sent freeze frame: 0x%02x, extras=%d", first, len(probeFrames))
	}
}

func TestThawStillSendsAfterCapabilityProbeFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var probes atomic.Int32
	frames := make(chan byte, 2)
	guest := fakeAgent(t, func(typ byte, _ []byte) (byte, []byte) {
		switch typ {
		case proto.INFO_REQ:
			if probes.Add(1) == 1 {
				return proto.INFO_RESP, []byte(`{"version":"v2.5.1","features":["fs_freeze"]}`)
			}
			return proto.ERROR, []byte("INFO unavailable")
		case proto.FREEZE_REQ:
			frames <- typ
			return proto.FREEZE_ACK, nil
		case proto.THAW_REQ:
			frames <- typ
			return proto.THAW_ACK, nil
		default:
			return oldAgentReply(typ, nil)
		}
	})
	if err := guest.Freeze(ctx, "/mnt/data"); err != nil {
		t.Fatalf("initial freeze: %v", err)
	}
	if _, err := guest.Info(ctx); err == nil {
		t.Fatal("INFO did not fail after freeze")
	}
	if err := guest.Thaw(ctx, "/mnt/data"); err != nil {
		t.Fatalf("THAW blocked by unrelated INFO failure: %v", err)
	}
	if first, second := <-frames, <-frames; first != proto.FREEZE_REQ || second != proto.THAW_REQ || probes.Load() != 2 {
		t.Fatalf("requests after failed INFO = 0x%02x, 0x%02x; INFO probes=%d", first, second, probes.Load())
	}
}
