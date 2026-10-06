package forward

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func dialTestWebSocket(t *testing.T, handler http.HandlerFunc) *WSConn {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	conn := NewWSConn(ws)
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestWSConnPreservesStreamAcrossBinaryMessages(t *testing.T) {
	got := make(chan string, 1)
	conn := dialTestWebSocket(t, func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for _, part := range []string{"", "abc", "defgh"} {
			if err := ws.WriteMessage(websocket.BinaryMessage, []byte(part)); err != nil {
				return
			}
		}
		var parts []string
		for range 2 {
			messageType, part, err := ws.ReadMessage()
			if err != nil || messageType != websocket.BinaryMessage {
				return
			}
			parts = append(parts, string(part))
		}
		got <- strings.Join(parts, "")
	})
	buf := make([]byte, 8)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "abcdefgh" {
		t.Fatalf("stream = %q, %v; want abcdefgh", buf, err)
	}
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte(" world")); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-got:
		if text != "hello world" {
			t.Fatalf("received %q, want hello world", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading websocket messages")
	}
}

func TestWSConnSerializesConcurrentWriters(t *testing.T) {
	const writers, perWriter = 12, 100
	result := make(chan map[string]bool, 1)
	conn := dialTestWebSocket(t, func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		ws.SetReadDeadline(time.Now().Add(10 * time.Second))
		seen := make(map[string]bool, writers*perWriter)
		for range writers * perWriter {
			messageType, payload, err := ws.ReadMessage()
			if err != nil || messageType != websocket.BinaryMessage {
				return
			}
			seen[string(payload)] = true
		}
		result <- seen
	})
	var wg sync.WaitGroup
	for writer := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range perWriter {
				payload := fmt.Appendf(nil, "%02d:%03d", writer, n)
				if _, err := conn.Write(payload); err != nil {
					t.Errorf("writer %d: %v", writer, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	select {
	case seen := <-result:
		if len(seen) != writers*perWriter {
			t.Fatalf("received %d distinct frames, want %d", len(seen), writers*perWriter)
		}
		for writer := range writers {
			for n := range perWriter {
				if !seen[fmt.Sprintf("%02d:%03d", writer, n)] {
					t.Fatalf("missing frame from writer %d, write %d", writer, n)
				}
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for concurrent websocket writes")
	}
}
