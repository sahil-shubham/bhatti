package gateway

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

type versionHandler struct{ calls atomic.Int32 }

func (h *versionHandler) SetSandbox(_, _ string, _ *EgressPolicy, _, _ string) error {
	h.calls.Add(1)
	return nil
}
func (h *versionHandler) DelSandbox(string) {}

func TestEnforcementVersionProbeAndAcknowledgedSet(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "bv-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "ctl.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	h := &versionHandler{}
	go ServeControl(ln, h)
	if err := ProbeVersion(ln.Addr().String()); err != nil {
		t.Fatal(err)
	}
	client := NewControlClient(ln.Addr().String())
	defer client.Close()
	if err := client.Send(ControlMsg{Op: ControlSet, GuestIP: "100.64.0.2", GuestMAC: "52:54:00:00:00:02", GuestToken: strings.Repeat("ab", 32), Policy: &NetPolicyWire{Default: "deny"}}); err != nil {
		t.Fatal(err)
	}
	if calls := h.calls.Load(); calls != 1 {
		t.Fatalf("SetSandbox acknowledged before it was applied (%d calls)", calls)
	}
}

func TestEnforcementVersionRejectsOldAndGarbledNetds(t *testing.T) {
	for _, tc := range []struct{ name, response string }{
		{"old netd ignores hello", ""},
		{"malformed answer", "not JSON\n"},
		{"wrong enforcement version", `{"version":999}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "bv-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			ln, err := net.Listen("unix", filepath.Join(dir, "ctl.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				var req ControlMsg
				_ = json.NewDecoder(c).Decode(&req)
				if tc.response != "" {
					_, _ = c.Write([]byte(tc.response))
					return
				}
				// The legacy control loop waits for the next request, not a reply.
				_ = json.NewDecoder(c).Decode(&req)
			}()
			if err := ProbeVersion(ln.Addr().String()); err == nil {
				t.Fatal("accepted old or malformed netd as enforcement-compatible")
			}
		})
	}
}
