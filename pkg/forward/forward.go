// Package forward bridges raw TCP streams and authenticated WebSocket tunnels.
package forward

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	pingInterval = 30 * time.Second
	pongTimeout  = 90 * time.Second
	writeTimeout = 10 * time.Second
)

// WSConn exposes binary WebSocket messages as one continuous byte stream.
// Exactly one goroutine may call Read; Write, ping and pong all share a writer
// lock, as required by gorilla/websocket.
type WSConn struct {
	conn      *websocket.Conn
	reader    io.Reader
	writeMu   sync.Mutex
	closeOnce sync.Once
	done      chan struct{}
}

// NewWSConn starts keepalives for a tunnel. Both peers use the same adapter,
// so idle connections survive reverse proxies without relying on TCP traffic.
func NewWSConn(conn *websocket.Conn) *WSConn {
	c := &WSConn{conn: conn, done: make(chan struct{})}
	conn.SetReadDeadline(time.Now().Add(pongTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongTimeout))
	})
	conn.SetPingHandler(func(payload string) error {
		if err := conn.SetReadDeadline(time.Now().Add(pongTimeout)); err != nil {
			return err
		}
		return c.writeControl(websocket.PongMessage, []byte(payload))
	})
	go c.ping()
	return c
}

func (c *WSConn) ping() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if err := c.writeControl(websocket.PingMessage, nil); err != nil {
				c.Close()
				return
			}
		}
	}
}

func (c *WSConn) writeControl(messageType int, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteControl(messageType, payload, time.Now().Add(writeTimeout))
}

func (c *WSConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if c.reader == nil {
			messageType, reader, err := c.conn.NextReader()
			if err != nil {
				return 0, err
			}
			if messageType != websocket.BinaryMessage {
				return 0, fmt.Errorf("unexpected tunnel websocket message type %d", messageType)
			}
			c.reader = reader
		}
		n, err := c.reader.Read(p)
		if errors.Is(err, io.EOF) {
			c.reader = nil
			if n == 0 {
				continue
			}
			return n, nil
		}
		return n, err
	}
}

func (c *WSConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return 0, err
	}
	if err := c.conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *WSConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		// A normal close keeps ordinary TCP EOFs from surfacing as noisy
		// "websocket: close 1006" errors in the client.
		c.writeControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	})
	return c.conn.Close()
}

// Abort immediately interrupts active reads and writes (e.g. on Ctrl-C).
// Closing the socket before waiting for a concurrent graceful Close also
// unblocks a writer stalled on a slow peer.
func (c *WSConn) Abort() error {
	err := c.conn.Close()
	c.closeOnce.Do(func() { close(c.done) })
	return err
}

// Relay copies raw bytes in both directions until either side ends, then
// closes both streams so the other copy goroutine cannot remain blocked.
func Relay(a, b io.ReadWriteCloser) error {
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(b, a)
		b.Close()
		done <- err
	}()
	_, errBA := io.Copy(a, b)
	a.Close()
	errAB := <-done
	if errBA != nil && !errors.Is(errBA, net.ErrClosed) {
		return errBA
	}
	if errAB != nil && !errors.Is(errAB, net.ErrClosed) {
		return errAB
	}
	return nil
}
