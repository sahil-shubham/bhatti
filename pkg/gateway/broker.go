package gateway

// This file is the wire protocol between netd and the credential broker (the
// daemon side, pkg/broker). netd parses untrusted guest traffic, so it holds
// no secrets: per intercepted TLS connection it asks the broker for what it
// needs, and the broker checks the grant every time. One unix socket per
// owner's netd; newline-delimited JSON, one response per request.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Broker operations.
const (
	// BrokerResolve asks for the value behind a placeholder, for a request to
	// Host from Sandbox. Answered with Value and TTLSeconds.
	BrokerResolve = "resolve"
	// BrokerCert asks for a leaf certificate for Host, signed by Sandbox's CA,
	// issued only while a live grant for Sandbox covers Host. Its existence is
	// what makes netd intercept a connection.
	BrokerCert = "cert"
	// BrokerReport records a refusal netd made itself (a placeholder outside
	// the headers, a Host header that isn't the intercepted host).
	BrokerReport = "report"
)

// BrokerMaxRequest bounds one request line, newline included.
const BrokerMaxRequest = 16 << 10

// brokerMaxResponse bounds one response line read by the client: a secret
// value or a certificate and key, never more.
const brokerMaxResponse = 64 << 10

// BrokerRequest is one request from netd.
type BrokerRequest struct {
	Op          string `json:"op"`
	Sandbox     string `json:"sandbox"`
	Placeholder string `json:"placeholder,omitempty"`
	Host        string `json:"host,omitempty"`
	Reason      string `json:"reason,omitempty"` // report
}

// BrokerResponse answers one request. Error set means refused.
type BrokerResponse struct {
	Value      string `json:"value,omitempty"`
	TTLSeconds int64  `json:"ttl_s,omitempty"` // resolve: seconds the value may be used for; 0 = while the connection lasts
	CertPEM    string `json:"cert,omitempty"`
	KeyPEM     string `json:"key,omitempty"`
	Error      string `json:"error,omitempty"`
}

// ErrBrokerMessageTooLarge is returned for a line longer than the reader's
// buffer.
var ErrBrokerMessageTooLarge = errors.New("broker: message too large")

// ReadBrokerMessage reads one newline-terminated JSON message from br into v.
// The bound is br's buffer size: size the reader with bufio.NewReaderSize.
func ReadBrokerMessage(br *bufio.Reader, v any) error {
	line, err := br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return ErrBrokerMessageTooLarge
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(line, v); err != nil {
		return fmt.Errorf("broker: decode: %w", err)
	}
	return nil
}

// WriteBrokerMessage writes v as one JSON line.
func WriteBrokerMessage(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("broker: encode: %w", err)
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// BrokerRefusal is the broker saying no (as opposed to not answering).
type BrokerRefusal struct{ Reason string }

func (e *BrokerRefusal) Error() string { return "broker refused: " + e.Reason }

// BrokerClient is netd's side: one short connection per request, so a broker
// restart (the daemon's) costs nothing but the requests made while it's down.
type BrokerClient struct {
	Path    string
	Timeout time.Duration // per request; 0 = 5s
}

// Do sends req and returns the broker's answer. A refusal is a *BrokerRefusal.
func (c *BrokerClient) Do(ctx context.Context, req BrokerRequest) (BrokerResponse, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Path)
	if err != nil {
		return BrokerResponse{}, fmt.Errorf("broker: dial: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := WriteBrokerMessage(conn, req); err != nil {
		return BrokerResponse{}, fmt.Errorf("broker: send: %w", err)
	}
	var resp BrokerResponse
	if err := ReadBrokerMessage(bufio.NewReaderSize(conn, brokerMaxResponse), &resp); err != nil {
		return BrokerResponse{}, fmt.Errorf("broker: receive: %w", err)
	}
	if resp.Error != "" {
		return resp, &BrokerRefusal{Reason: resp.Error}
	}
	return resp, nil
}
