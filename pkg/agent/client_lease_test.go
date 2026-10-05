package agent

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// leaseTestAgent handles INFO on its own connection and leaves FREEZE's
// connection open. THAW on a different connection is a protocol failure.
func leaseTestAgent(t *testing.T, lease func(net.Conn)) *AgentClient {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bh-lease-")
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
				typ, _, err := proto.ReadFrame(conn)
				if err != nil {
					return
				}
				switch typ {
				case proto.INFO_REQ:
					proto.WriteFrame(conn, proto.INFO_RESP, []byte(`{"version":"v2.5.1","features":["fs_freeze"]}`))
				case proto.FREEZE_REQ:
					lease(conn)
				default:
					proto.WriteFrame(conn, proto.ERROR, []byte("THAW sent on different connection"))
				}
			}()
		}
	}()
	return NewTestClient(path, path)
}

func TestFreezeLeaseRetainsConnectionAfterFreezeDeadline(t *testing.T) {
	frames := make(chan byte, 1)
	guest := leaseTestAgent(t, func(conn net.Conn) {
		if err := proto.WriteFrame(conn, proto.FREEZE_ACK, nil); err != nil {
			return
		}
		typ, payload, err := proto.ReadFrame(conn)
		if err != nil {
			return
		}
		if string(payload) != `{"mount":"/mnt/data"}` {
			frames <- 0
			return
		}
		frames <- typ
		proto.WriteFrame(conn, proto.THAW_ACK, nil)
	})
	freezeCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := guest.Freeze(freezeCtx, "/mnt/data/"); err != nil {
		t.Fatal(err)
	}
	<-freezeCtx.Done() // Thaw must not inherit the now-expired freeze deadline.
	thawCtx, thawCancel := context.WithTimeout(context.Background(), time.Second)
	defer thawCancel()
	if err := guest.Thaw(thawCtx, "/mnt/data"); err != nil {
		t.Fatalf("retained lease thaw: %v", err)
	}
	select {
	case typ := <-frames:
		if typ != proto.THAW_REQ {
			t.Fatalf("lease received 0x%02x, want THAW_REQ", typ)
		}
	case <-thawCtx.Done():
		t.Fatal("no THAW_REQ on the original freeze connection")
	}
}

func TestThawWithoutAckClosesLeaseAndReportsUnconfirmedRelease(t *testing.T) {
	closed := make(chan struct{})
	guest := leaseTestAgent(t, func(conn net.Conn) {
		if proto.WriteFrame(conn, proto.FREEZE_ACK, nil) != nil {
			return
		}
		if typ, _, err := proto.ReadFrame(conn); err != nil || typ != proto.THAW_REQ {
			return
		}
		proto.WriteFrame(conn, proto.ERROR, []byte("FITHAW: input/output error"))
		if typ, _, err := proto.ReadFrame(conn); err != nil && typ == 0 {
			close(closed)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := guest.Freeze(ctx, "/mnt/data"); err != nil {
		t.Fatal(err)
	}
	if err := guest.Thaw(ctx, "/mnt/data"); !errors.Is(err, ErrFSFreezeReleaseUnconfirmed) {
		t.Fatalf("thaw without ACK = %v, want unconfirmed sentinel", err)
	}
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("failed THAW left freeze connection open")
	}
}

func TestTimedOutFreezeClosesLeaseForGuestAutoThaw(t *testing.T) {
	closed := make(chan struct{})
	guest := leaseTestAgent(t, func(conn net.Conn) {
		if _, _, err := proto.ReadFrame(conn); err != nil {
			close(closed)
		}
	})
	if _, err := guest.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := guest.Freeze(ctx, "/mnt/data"); err == nil || errors.Is(err, ErrFSFreezeRejected) {
		t.Fatalf("unacknowledged freeze = %v; want ambiguous timeout requiring fallback thaw", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("timed-out FREEZE kept the connection open")
	}
}
