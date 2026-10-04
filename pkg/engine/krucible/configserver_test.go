//go:build krucible

package krucible

import (
	"bytes"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/configdrive"
)

// fetchOverUDS mimics exactly what lohar does at boot: dial the config UDS,
// send CONFIG_REQ, read the CONFIG_RESP frame. Returns the raw payload.
func fetchOverUDS(t *testing.T, uds string) (byte, []byte) {
	t.Helper()
	conn, err := net.DialTimeout("unix", uds, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", uds, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := proto.WriteFrame(conn, proto.CONFIG_REQ, nil); err != nil {
		t.Fatalf("write CONFIG_REQ: %v", err)
	}
	typ, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read CONFIG_RESP: %v", err)
	}
	return typ, payload
}

func serveConfig(t *testing.T, cfg configdrive.SandboxConfig) string {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	uds := filepath.Join(shortSockDir(t), "cfg.sock")
	srv, err := newConfigServer(uds, raw)
	if err != nil {
		t.Fatalf("newConfigServer: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return uds
}

// TestConfigServerRoundTrip: the server hands back exactly the config it was
// given, framed as CONFIG_RESP, and lohar's parse recovers every field. This is
// the boot-time delivery contract that replaces the config drive.
func TestConfigServerRoundTrip(t *testing.T) {
	want := configdrive.SandboxConfig{
		SandboxID: "sb_abc",
		Hostname:  "dev",
		Token:     "tok_secret_123",
		Env:       map[string]string{"FOO": "bar", "OPENAI_API_KEY": "sk-xyz"},
		DNS:       []string{"1.1.1.1"},
	}
	uds := serveConfig(t, want)

	typ, payload := fetchOverUDS(t, uds)
	if typ != proto.CONFIG_RESP {
		t.Fatalf("frame type = 0x%02x, want CONFIG_RESP (0x%02x)", typ, proto.CONFIG_RESP)
	}
	var got configdrive.SandboxConfig
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if got.Token != want.Token {
		t.Errorf("token = %q, want %q", got.Token, want.Token)
	}
	if got.Hostname != want.Hostname {
		t.Errorf("hostname = %q, want %q", got.Hostname, want.Hostname)
	}
	if got.Env["FOO"] != "bar" || got.Env["OPENAI_API_KEY"] != "sk-xyz" {
		t.Errorf("env = %v, want FOO=bar OPENAI_API_KEY=sk-xyz", got.Env)
	}
}

// TestConfigServerIsolation: two servers each serve THEIR OWN config. This is
// the per-sandbox capability property — a guest reaching its UDS gets its
// config, never another sandbox's. (A server that ignored its payload and
// served a shared/global blob would pass RoundTrip but fail here.)
func TestConfigServerIsolation(t *testing.T) {
	udsA := serveConfig(t, configdrive.SandboxConfig{SandboxID: "A", Token: "tok_A"})
	udsB := serveConfig(t, configdrive.SandboxConfig{SandboxID: "B", Token: "tok_B"})

	for _, tc := range []struct{ uds, wantTok, wantID string }{
		{udsA, "tok_A", "A"},
		{udsB, "tok_B", "B"},
	} {
		_, payload := fetchOverUDS(t, tc.uds)
		var got configdrive.SandboxConfig
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Token != tc.wantTok || got.SandboxID != tc.wantID {
			t.Errorf("%s served {id:%q tok:%q}, want {id:%q tok:%q}", tc.uds, got.SandboxID, got.Token, tc.wantID, tc.wantTok)
		}
	}
}

// TestConfigServerSeedsEntropy: every answer carries its own 256-bit seed for
// the guest's CRNG, alongside the config as given — fields this daemon doesn't
// know included. The seed is all a guest without a hardware RNG has to seed it
// at boot (TestKrucibleGuestCRNGSeededAtBoot).
func TestConfigServerSeedsEntropy(t *testing.T) {
	uds := filepath.Join(shortSockDir(t), "cfg.sock")
	srv, err := newConfigServer(uds, []byte(`{"token":"tok_A","from_a_newer_daemon":{"x":1}}`))
	if err != nil {
		t.Fatalf("newConfigServer: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	var seeds [][]byte
	for range 2 {
		_, payload := fetchOverUDS(t, uds)
		var got configdrive.SandboxConfig
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Token != "tok_A" || !bytes.Contains(payload, []byte(`"from_a_newer_daemon":{"x":1}`)) {
			t.Fatalf("config not served as given: %s", payload)
		}
		if len(got.Entropy) != configEntropyLen || bytes.Equal(got.Entropy, make([]byte, configEntropyLen)) {
			t.Fatalf("entropy = %x, want %d random bytes", got.Entropy, configEntropyLen)
		}
		seeds = append(seeds, got.Entropy)
	}
	if bytes.Equal(seeds[0], seeds[1]) {
		t.Fatal("two boots were sent the same seed")
	}
}

// TestConfigServerWaitsForGuestClose: after answering, the server must leave the
// connection open until the guest hangs up. libkrun maps a host-side close to a
// vsock RST, which makes the guest kernel drop the unread CONFIG_RESP — on a
// fast host lohar then boots with no config (seen on an x86 Hetzner box).
func TestConfigServerWaitsForGuestClose(t *testing.T) {
	uds := serveConfig(t, configdrive.SandboxConfig{Token: "x"})
	conn, err := net.DialTimeout("unix", uds, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := proto.WriteFrame(conn, proto.CONFIG_REQ, nil); err != nil {
		t.Fatalf("write CONFIG_REQ: %v", err)
	}
	if typ, _, err := proto.ReadFrame(conn); err != nil || typ != proto.CONFIG_RESP {
		t.Fatalf("read CONFIG_RESP: typ=%#x err=%v", typ, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	var b [1]byte
	_, err = conn.Read(b[:])
	if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("server closed first (err=%v); must wait for the guest to hang up", err)
	}
}

// TestConfigServerNoLeakOnBadFrame: a connection that does NOT open with
// CONFIG_REQ gets no response — the server never emits the config (which holds
// the token) on an unexpected/garbage opening frame.
func TestConfigServerNoLeakOnBadFrame(t *testing.T) {
	uds := serveConfig(t, configdrive.SandboxConfig{Token: "tok_secret"})
	conn, err := net.DialTimeout("unix", uds, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// Wrong opening frame (STDOUT instead of CONFIG_REQ).
	if err := proto.WriteFrame(conn, proto.STDOUT, []byte("junk")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if typ, payload, err := proto.ReadFrame(conn); err == nil {
		t.Fatalf("server responded to a non-CONFIG_REQ frame: type=0x%02x payload=%q (config must not leak)", typ, payload)
	}
}

// TestConfigServerClose: after Close(), the socket no longer serves.
func TestConfigServerClose(t *testing.T) {
	uds := filepath.Join(shortSockDir(t), "cfg.sock")
	srv, err := newConfigServer(uds, []byte(`{"token":"x"}`))
	if err != nil {
		t.Fatalf("newConfigServer: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := srv.Close(); err != nil { // idempotent
		t.Fatalf("second close: %v", err)
	}
	if _, err := net.DialTimeout("unix", uds, 200*time.Millisecond); err == nil {
		t.Fatalf("dial succeeded after Close()")
	}
}
