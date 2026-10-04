package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// fakePiped replays a fixed sequence of agent frames, then EOF.
type fakePiped struct{ frames [][2][]byte }

func (f *fakePiped) ReadFrame() (byte, []byte, error) {
	if len(f.frames) == 0 {
		return 0, nil, io.EOF
	}
	fr := f.frames[0]
	f.frames = f.frames[1:]
	return fr[0][0], fr[1], nil
}
func (f *fakePiped) WriteStdin([]byte) error { return nil }
func (f *fakePiped) Kill() error             { return nil }
func (f *fakePiped) Close() error            { return nil }

type wsMsg struct {
	binary bool
	data   string
}

// relayOutput runs pipedWSRelay over a real WebSocket and returns what the
// client received, in order.
func relayOutput(t *testing.T, streams *streamSet) []wsMsg {
	t.Helper()
	exit := proto.ExitPayload(0)
	pc := &fakePiped{frames: [][2][]byte{
		{{proto.STDOUT}, []byte("out")},
		{{proto.STDERR}, []byte("err")},
		{{proto.EXIT}, exit[:]},
	}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		pipedWSRelay(conn, pc, streams)
	}))
	defer ts.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	var got []wsMsg
	for {
		typ, data, err := ws.ReadMessage()
		if err != nil {
			return got
		}
		got = append(got, wsMsg{binary: typ == websocket.BinaryMessage, data: string(data)})
	}
}

// With streams chosen, each subscribed stream arrives as a binary frame tagged
// with its id, an unsubscribed one not at all, and text frames carry only the
// control JSON. Without, everything is text, as before.
func TestPipedRelayStreams(t *testing.T) {
	isExit := func(m wsMsg) bool {
		var v struct{ Type string }
		return !m.binary && json.Unmarshal([]byte(m.data), &v) == nil && v.Type == "exit"
	}

	got := relayOutput(t, &streamSet{stdout: true, stderr: true})
	if len(got) != 3 || got[0] != (wsMsg{true, "\x01out"}) || got[1] != (wsMsg{true, "\x02err"}) || !isExit(got[2]) {
		t.Fatalf("both streams: %+v", got)
	}

	got = relayOutput(t, &streamSet{stdout: true})
	if len(got) != 2 || got[0] != (wsMsg{true, "\x01out"}) || !isExit(got[1]) {
		t.Fatalf("stdout only: %+v", got)
	}

	got = relayOutput(t, nil)
	if len(got) != 3 || got[0] != (wsMsg{false, "out"}) || got[1] != (wsMsg{false, "err"}) || !isExit(got[2]) {
		t.Fatalf("legacy: %+v", got)
	}
}
