//go:build linux

package main

import (
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// stubVsock swaps dialVsock's syscalls for ones that hand out unix sockets and
// answer connect from script (one entry per call; the last repeats). It
// returns how many times connect was called.
func stubVsock(t *testing.T, script ...syscall.Errno) *int {
	t.Helper()
	origSocket, origConnect := vsockSocket, vsockConnect
	t.Cleanup(func() { vsockSocket, vsockConnect = origSocket, origConnect })
	calls := new(int)
	vsockSocket = func() (int, error) {
		return syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	}
	vsockConnect = func(int, *SockaddrVM) syscall.Errno {
		i := min(*calls, len(script)-1)
		*calls++
		return script[i]
	}
	return calls
}

// A signal landing while connect(2) waits for the host fails it with EINTR,
// and the kernel has dropped the attempt: lohar used to give up on the config
// there, boot without its token, and never answer the host.
func TestDialVsockRetriesAnInterruptedConnect(t *testing.T) {
	calls := stubVsock(t, syscall.EINTR, syscall.EINTR, 0)
	conn, err := dialVsock(1026)
	if err != nil {
		t.Fatalf("dialVsock: %v", err)
	}
	conn.Close()
	if *calls != 3 {
		t.Fatalf("connect called %d times, want 3 (two interrupted, one good)", *calls)
	}
}

func TestDialVsockOnlyRetriesInterruptedConnects(t *testing.T) {
	calls := stubVsock(t, syscall.ECONNRESET)
	if _, err := dialVsock(1026); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("dialVsock = %v, want ECONNRESET", err)
	}
	if *calls != 1 {
		t.Fatalf("connect called %d times for a refused connection, want 1", *calls)
	}

	calls = stubVsock(t, syscall.EINTR)
	if _, err := dialVsock(1026); !errors.Is(err, syscall.EINTR) {
		t.Fatalf("dialVsock = %v, want EINTR once it gives up", err)
	}
	if *calls != vsockConnectTries {
		t.Fatalf("connect called %d times, want the %d-try bound", *calls, vsockConnectTries)
	}
}

// os.File applies deadlines only to a pollable fd; on a blocking one they fail
// quietly, so a host that stopped answering would hang lohar's boot for good.
func TestDialedVsockConnHonorsDeadlines(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	peer := os.NewFile(uintptr(fds[1]), "peer")
	defer peer.Close()
	origSocket, origConnect := vsockSocket, vsockConnect
	t.Cleanup(func() { vsockSocket, vsockConnect = origSocket, origConnect })
	vsockSocket = func() (int, error) { return fds[0], nil }
	vsockConnect = func(int, *SockaddrVM) syscall.Errno { return 0 }

	conn, err := dialVsock(1026)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Read = %v, want a deadline error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read ignored its deadline")
	}
}

var quickFetch = configFetchPolicy{attempts: 5, backoff: time.Millisecond, budget: time.Minute, exchange: time.Second}

// host answers one connection the way the daemon's config server does.
func host(reply func(net.Conn)) func() (net.Conn, error) {
	return func() (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			if typ, _, err := proto.ReadFrame(server); err != nil || typ != proto.CONFIG_REQ {
				return
			}
			reply(server)
		}()
		return client, nil
	}
}

func serveConfig(conn net.Conn) {
	proto.WriteFrame(conn, proto.CONFIG_RESP, []byte(`{"sandbox_id":"sb1","token":"tok"}`))
	io.Copy(io.Discard, conn) // like the real server: stay until the guest hangs up
}

// lohar fetches its config once. Whatever fails a single exchange — an error
// at connect, the host hanging up unanswered, a host that goes quiet — must
// cost an attempt, not the guest its token and network.
func TestFetchConfigRetriesFailedExchanges(t *testing.T) {
	attempts := 0
	dials := []func() (net.Conn, error){
		func() (net.Conn, error) { return nil, syscall.ECONNRESET },
		host(func(net.Conn) {}),                              // reads the request, hangs up
		host(func(net.Conn) { time.Sleep(2 * time.Second) }), // reads the request, goes quiet
		host(serveConfig),
	}
	p := quickFetch
	p.exchange = 300 * time.Millisecond
	cfg := fetchConfigVia(func() (net.Conn, error) {
		dial := dials[attempts]
		attempts++
		return dial()
	}, p)
	if cfg == nil || cfg.Token != "tok" || cfg.SandboxID != "sb1" {
		t.Fatalf("config = %+v after %d attempts, want sb1's", cfg, attempts)
	}
	if attempts != len(dials) {
		t.Fatalf("%d attempts, want %d", attempts, len(dials))
	}
}

func TestFetchConfigGivesUpWhenTheHostNeverAnswers(t *testing.T) {
	attempts := 0
	p := quickFetch
	p.attempts = 3
	if cfg := fetchConfigVia(func() (net.Conn, error) { attempts++; return nil, syscall.ETIMEDOUT }, p); cfg != nil {
		t.Fatalf("config = %+v from a host that never answered", cfg)
	}
	if attempts != 3 {
		t.Fatalf("%d attempts, want 3", attempts)
	}
}
