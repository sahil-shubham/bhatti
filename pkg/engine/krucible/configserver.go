package krucible

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"os"
	"sync"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// configServer serves a sandbox's boot config over a guest→host vsock UDS.
//
// libkrun (in the per-VM helper) forwards the guest's connection on
// proto.VsockPortConfig to this UDS (krun_add_vsock_port2 listen=false); lohar
// dials it once at boot to fetch its SandboxConfig — replacing the on-disk
// config drive (DESIGN-bhatti-v2-secrets-and-trust §3.4). Nothing is written to
// a guest disk or captured in a snapshot.
//
// The UDS is per-sandbox, so the channel *is* the capability: a guest reaches
// only its own server and thus only its own config — no guest-presented
// credential, no cross-tenant reach (§3.1).
//
// Each answer also carries a fresh seed for the guest kernel's CRNG
// (configdrive.SandboxConfig.Entropy), which lohar credits before it starts
// anything: a guest with no hardware RNG has nothing else to seed it for
// minutes. It's made per request, so no two boots share one and none is ever
// written down.
type configServer struct {
	ln     net.Listener
	config map[string]json.RawMessage // configdrive.SandboxConfig, field by field

	mu     sync.Mutex
	closed bool
}

// configEntropyLen is 256 bits: what the guest kernel needs to consider its
// CRNG seeded.
const configEntropyLen = 32

// newConfigServer starts serving payload (the config JSON) on udsPath and
// returns once the socket is listening (so the caller can spawn the VM knowing
// a guest dial won't race a not-yet-bound socket).
func newConfigServer(udsPath string, payload []byte) (*configServer, error) {
	// Kept as raw fields, not decoded into the struct, so a field this daemon
	// doesn't know still reaches the guest.
	var config map[string]json.RawMessage
	if err := json.Unmarshal(payload, &config); err != nil {
		return nil, err
	}
	if config == nil {
		return nil, errors.New("config is not a JSON object")
	}
	_ = os.Remove(udsPath) // clear a stale socket from a prior incarnation
	ln, err := net.Listen("unix", udsPath)
	if err != nil {
		return nil, err
	}
	s := &configServer{ln: ln, config: config}
	go s.serve()
	return s, nil
}

func (s *configServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go s.handle(conn)
	}
}

// handle answers one CONFIG_REQ with a CONFIG_RESP carrying the config JSON.
// A connection that doesn't open with CONFIG_REQ gets nothing (we never serve
// on an unexpected frame).
//
// After writing, it waits for the guest to hang up before closing. libkrun's
// unix proxy turns a host-side close into a vsock RST, and the guest kernel
// discards unread data on RST — so closing right after the write races lohar's
// read and, on a fast host, lohar sees EOF and boots without its config.
func (s *configServer) handle(conn net.Conn) {
	defer conn.Close()
	msgType, _, err := proto.ReadFrame(conn)
	if err != nil || msgType != proto.CONFIG_REQ {
		return
	}
	if proto.WriteFrame(conn, proto.CONFIG_RESP, s.response()) != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(configLinger))
	_, _ = io.Copy(io.Discard, conn)
}

// response is the config with a fresh entropy seed in it. Neither Marshal can
// fail: the seed is bytes, every other field was parsed from JSON.
func (s *configServer) response() []byte {
	seed := make([]byte, configEntropyLen)
	rand.Read(seed)
	resp := maps.Clone(s.config)
	resp["entropy"], _ = json.Marshal(seed)
	b, _ := json.Marshal(resp)
	return b
}

// configLinger bounds how long a served connection is held open waiting for the
// guest to close it (lohar closes as soon as it has read the response).
const configLinger = 5 * time.Second

// Close stops the server. Idempotent.
func (s *configServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.ln.Close()
}
