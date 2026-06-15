package krucible

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// controlCmd sends a single newline command to the libkrun control socket and
// reads one newline-terminated reply. The protocol is one-shot per connection:
//
//	PAUSE  -> OK paused | ERR <reason>
//	RESUME -> OK running | ERR <reason>
//	STATUS -> OK <state>  (state ∈ running|pausing|paused|resuming)
//
// See `krun_set_control_socket` in libkrucible.
func controlCmd(ctx context.Context, uds, cmd string) (string, error) {
	if uds == "" {
		return "", fmt.Errorf("no control socket configured")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(3 * time.Second)
	}
	var d net.Dialer
	d.Deadline = deadline
	conn, err := d.DialContext(ctx, "unix", uds)
	if err != nil {
		return "", fmt.Errorf("dial control socket: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		return "", fmt.Errorf("write %q: %w", cmd, err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read reply: %w", err)
	}
	reply = strings.TrimRight(reply, "\r\n")
	if strings.HasPrefix(reply, "ERR ") {
		return "", fmt.Errorf("control %s: %s", cmd, strings.TrimPrefix(reply, "ERR "))
	}
	if !strings.HasPrefix(reply, "OK") {
		return "", fmt.Errorf("control %s: unexpected reply %q", cmd, reply)
	}
	// Strip "OK " (or "OK" alone) — return whatever follows.
	return strings.TrimSpace(strings.TrimPrefix(reply, "OK")), nil
}
