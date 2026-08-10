package krucible

import (
	"net"
	"os"
	"sync"

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
type configServer struct {
	ln      net.Listener
	payload []byte // marshaled configdrive.SandboxConfig JSON

	mu     sync.Mutex
	closed bool
}

// newConfigServer starts serving payload on udsPath and returns once the socket
// is listening (so the caller can spawn the VM knowing a guest dial won't race a
// not-yet-bound socket).
func newConfigServer(udsPath string, payload []byte) (*configServer, error) {
	_ = os.Remove(udsPath) // clear a stale socket from a prior incarnation
	ln, err := net.Listen("unix", udsPath)
	if err != nil {
		return nil, err
	}
	s := &configServer{ln: ln, payload: payload}
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
func (s *configServer) handle(conn net.Conn) {
	defer conn.Close()
	msgType, _, err := proto.ReadFrame(conn)
	if err != nil || msgType != proto.CONFIG_REQ {
		return
	}
	_ = proto.WriteFrame(conn, proto.CONFIG_RESP, s.payload)
}

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
