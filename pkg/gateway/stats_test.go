package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// listenShort listens on a unix socket under /tmp: t.TempDir() on macOS is
// longer than an AF_UNIX path may be.
func listenShort(t *testing.T) (net.Listener, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bst-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "ctl.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln, ln.Addr().String()
}

// plainHandler is a ControlHandler with nothing to report, the shape of every
// handler written before the stats op existed.
type plainHandler struct{ sets, dels atomic.Int32 }

func (h *plainHandler) SetSandbox(string, string, *EgressPolicy, string, string) error {
	h.sets.Add(1)
	return nil
}
func (h *plainHandler) DelSandbox(string) { h.dels.Add(1) }

// reportingHandler is a plainHandler that also answers stats.
type reportingHandler struct {
	plainHandler
	report NetStats
}

func (h *reportingHandler) Stats() NetStats { return h.report }

func serveHandler(t *testing.T, h ControlHandler) string {
	t.Helper()
	ln, path := listenShort(t)
	go ServeControl(ln, h)
	return path
}

// exchange sends each request line on one connection and returns the reply
// line to each, so a test sees the wire as a client does.
func exchange(t *testing.T, path string, requests ...string) []string {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	r := bufio.NewReader(conn)
	var replies []string
	for _, req := range requests {
		if _, err := conn.Write([]byte(req + "\n")); err != nil {
			t.Fatal(err)
		}
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("no reply to %s: %v", req, err)
		}
		replies = append(replies, strings.TrimSpace(line))
	}
	return replies
}

func TestQueryStatsReturnsWhatTheHandlerReports(t *testing.T) {
	want := NetStats{Guests: []GuestStats{
		{GuestIP: "100.64.0.2", SandboxID: "sb-a", RxBytes: 1500, RxPackets: 3, TxBytes: 900, TxPackets: 2,
			Dropped: DropStats{Policy: 4, Spoof: 5, HardDeny: 6}},
		{GuestIP: "100.64.0.9", SandboxID: "sb-b", RxBytes: 7, RxPackets: 8, TxBytes: 9, TxPackets: 10},
	}}
	path := serveHandler(t, &reportingHandler{report: want})
	got, err := QueryStats(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("QueryStats = %+v, want %+v", got, want)
	}
}

// The collector ranges over Guests and a JSON consumer indexes it, so a netd
// with no guests must say [] and not null, whatever its handler returned.
func TestAnEmptyReportIsAnEmptyListOnTheWire(t *testing.T) {
	path := serveHandler(t, &reportingHandler{}) // a nil Guests slice
	replies := exchange(t, path, `{"op":"stats"}`)
	if !strings.Contains(replies[0], `"guests":[]`) {
		t.Fatalf("empty report on the wire = %s, want \"guests\":[]", replies[0])
	}
	got, err := QueryStats(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Guests == nil || len(got.Guests) != 0 {
		t.Fatalf("QueryStats of no guests = %#v, want an empty non-nil list", got.Guests)
	}
}

// The tag names are the contract between a daemon and a netd of another build:
// a renamed tag would read as zeroes on the other side, not as an error.
func TestStatsWireContract(t *testing.T) {
	const wire = `{"guests":[{"guest_ip":"100.64.0.2","sandbox_id":"sb","rx_bytes":1,"rx_packets":2,` +
		`"tx_bytes":3,"tx_packets":4,"dropped":{"policy":5,"spoof":6,"hard_deny":7}}]}`
	want := NetStats{Guests: []GuestStats{{GuestIP: "100.64.0.2", SandboxID: "sb", RxBytes: 1, RxPackets: 2,
		TxBytes: 3, TxPackets: 4, Dropped: DropStats{Policy: 5, Spoof: 6, HardDeny: 7}}}}
	var got NetStats
	if err := json.Unmarshal([]byte(wire), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded %+v, want %+v", got, want)
	}
	enc, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(enc) != wire {
		t.Fatalf("encoded %s, want %s", enc, wire)
	}
}

// A handler written before stats existed does not implement StatsProvider. It
// must answer the op like the netds already deployed do, leave its guests
// alone, and keep serving the rest of the protocol on the same connection.
func TestHandlerWithoutStatsAnswersAsAnOldNetdDoes(t *testing.T) {
	h := &plainHandler{}
	path := serveHandler(t, h)
	set := `{"op":"set","guest_ip":"100.64.0.2","guest_mac":"52:54:00:00:00:02","guest_token":"` +
		strings.Repeat("ab", 32) + `","policy":{"default":"deny"}}`
	replies := exchange(t, path, `{"op":"hello"}`, `{"op":"stats"}`, set, `{"op":"bogus"}`, `{"op":"del","guest_ip":"100.64.0.2"}`)
	for i, want := range []string{
		`{"version":1}`,
		`{"version":1,"error":"unknown control operation"}`, // what a deployed netd writes for any op it lacks
		`{"version":1}`,
		`{"version":1,"error":"unknown control operation"}`,
		`{"version":1}`,
	} {
		if replies[i] != want {
			t.Errorf("reply %d = %s, want %s", i, replies[i], want)
		}
	}
	if h.sets.Load() != 1 || h.dels.Load() != 1 {
		t.Fatalf("handler saw %d sets and %d dels; the stats op must not touch registrations (want 1 and 1)", h.sets.Load(), h.dels.Load())
	}

	_, err := QueryStats(context.Background(), path)
	if !errors.Is(err, ErrStatsUnsupported) {
		t.Fatalf("QueryStats against a handler with no report = %v, want ErrStatsUnsupported", err)
	}
	if err := ProbeVersion(path); err != nil {
		t.Fatalf("version probe after a stats query: %v", err)
	}
}

// fakeNetd answers one control request with canned bytes, except "silent" (read
// the request, never answer) and "close" (hang up without answering).
func fakeNetd(t *testing.T, reply string) (path string, request <-chan ControlMsg) {
	t.Helper()
	ln, path := listenShort(t)
	got := make(chan ControlMsg, 1)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var m ControlMsg
		if err := json.NewDecoder(conn).Decode(&m); err != nil {
			return
		}
		got <- m
		switch reply {
		case "silent":
			<-done
		case "close":
		default:
			_, _ = conn.Write([]byte(reply))
		}
	}()
	return path, got
}

// The literal below is the text a netd built before the stats op writes, so
// this fails if the client and the server drift apart on it.
func TestQueryStatsRecognisesANetdThatPredatesStats(t *testing.T) {
	path, request := fakeNetd(t, `{"version":1,"error":"unknown control operation"}`+"\n")
	_, err := QueryStats(context.Background(), path)
	if !errors.Is(err, ErrStatsUnsupported) {
		t.Fatalf("QueryStats = %v, want ErrStatsUnsupported", err)
	}
	if m := <-request; m.Op != "stats" {
		t.Fatalf("the netd was sent op %q, want \"stats\"", m.Op)
	}
}

func TestQueryStatsFailsOnAnswersThatAreNotAReport(t *testing.T) {
	for _, tc := range []struct{ name, reply string }{
		{"acknowledgement without stats", `{"version":1}` + "\n"},
		{"netd error", `{"version":1,"error":"boom"}` + "\n"},
		{"not JSON", "not JSON\n"},
		{"hangs up", "close"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, _ := fakeNetd(t, tc.reply)
			_, err := QueryStats(context.Background(), path)
			if err == nil {
				t.Fatal("QueryStats accepted an answer that carries no report")
			}
			if errors.Is(err, ErrStatsUnsupported) {
				t.Fatalf("QueryStats = %v: only a netd that does not know the op is unsupported", err)
			}
		})
	}
}

// The collector polls every owner's netd in one pass: a netd that accepts and
// never answers may cost it a second, not the pass. The bounds are loose on
// purpose (the machine may be busy); they only have to tell the caller's
// deadline from the 1s cap, and the cap from no cap at all.
func TestQueryStatsGivesUpOnASilentNetd(t *testing.T) {
	t.Run("caller's deadline", func(t *testing.T) {
		path, _ := fakeNetd(t, "silent")
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := QueryStats(ctx, path)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("QueryStats = %v, want the context's deadline", err)
		}
		if d := time.Since(start); d > 900*time.Millisecond {
			t.Fatalf("returned after %v for a 50ms deadline", d)
		}
	})
	t.Run("caller cancels", func(t *testing.T) {
		path, _ := fakeNetd(t, "silent")
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		start := time.Now()
		_, err := QueryStats(ctx, path)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("QueryStats = %v, want context.Canceled", err)
		}
		if d := time.Since(start); d > 900*time.Millisecond {
			t.Fatalf("returned %v after cancellation was due at 50ms", d)
		}
	})
	t.Run("internal cap", func(t *testing.T) {
		path, _ := fakeNetd(t, "silent")
		start := time.Now()
		_, err := QueryStats(context.Background(), path) // no deadline of the caller's own
		d := time.Since(start)
		if err == nil {
			t.Fatal("QueryStats returned a report from a netd that never answered")
		}
		if d < 800*time.Millisecond || d > 5*time.Second {
			t.Fatalf("gave up after %v, want about the 1s cap", d)
		}
	})
}

func TestQueryStatsFailsFastWhenNobodyListens(t *testing.T) {
	ln, path := listenShort(t)
	unix := ln.(*net.UnixListener)
	unix.SetUnlinkOnClose(false) // a dead netd leaves its socket file behind
	ln.Close()
	for name, p := range map[string]string{
		"stale socket file": path,
		"no socket at all":  filepath.Join(filepath.Dir(path), "gone.sock"),
	} {
		start := time.Now()
		_, err := QueryStats(context.Background(), p)
		if err == nil || errors.Is(err, ErrStatsUnsupported) {
			t.Errorf("%s: QueryStats = %v, want a connection error", name, err)
		}
		// Refused at once, not after the 1s cap.
		if d := time.Since(start); d > 900*time.Millisecond {
			t.Errorf("%s: took %v to fail", name, d)
		}
	}
}

// The connection's deadline is ctx's deadline, and the two timers fire in no
// fixed order: a read can time out an instant before ctx.Err() says so. The
// caller set a deadline and has to be told it passed, whichever got there first.
func TestQueryStatsReportsTheCallersDeadlineWhenTheReadTimesOutFirst(t *testing.T) {
	timedOut := &net.OpError{Op: "read", Net: "unix", Err: os.ErrDeadlineExceeded}
	if err := statsErr(context.Background(), timedOut); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a read that timed out before ctx noted its deadline = %v, want context.DeadlineExceeded", err)
	}
	// A cancellation the context has noted wins over the i/o error it caused.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := statsErr(ctx, timedOut); !errors.Is(err, context.Canceled) {
		t.Errorf("a read woken by cancellation = %v, want context.Canceled", err)
	}
	// An error that is not a timeout keeps its own identity.
	refused := &net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNREFUSED}
	if err := statsErr(context.Background(), refused); !errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a refused connection = %v, want ECONNREFUSED and no deadline", err)
	}
}
