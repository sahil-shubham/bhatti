package proto

// Frame types for the guest agent protocol.
//
// All communication between the bhatti host process and a guest VM happens
// over vsock using a binary framing protocol. This protocol is
// engine-independent — it can be tested over net.Pipe() or a Unix socket
// without any VM.
//
// Reference: https://github.com/superhq-ai/shuru
const (
	// I/O streams
	STDIN  byte = 0x01 // host → guest: bytes for child stdin
	STDOUT byte = 0x02 // guest → host: child stdout bytes
	STDERR byte = 0x03 // guest → host: child stderr bytes

	// Control
	RESIZE byte = 0x04 // host → guest: [u16 rows][u16 cols] big-endian (4 bytes exactly)
	EXIT   byte = 0x05 // guest → host: [i32 exit_code] big-endian (4 bytes exactly)
	ERROR  byte = 0x06 // either direction: UTF-8 error message (variable length)
	KILL   byte = 0x07 // host → guest: empty payload, agent sends SIGTERM to child

	// Exec
	EXEC_REQ byte = 0x10 // host → guest: JSON-encoded ExecRequest

	// Auth
	AUTH byte = 0x11 // host → guest: token bytes (first frame after connect)

	// Config fetch (guest → host, boot-time). lohar pulls its SandboxConfig over
	// vsock instead of reading an on-disk config drive (DESIGN §3.4). The UDS the
	// host serves this on is per-sandbox, so the channel is the capability: a
	// guest can only fetch its own config.
	CONFIG_REQ  byte = 0x12 // guest → host: empty payload
	CONFIG_RESP byte = 0x13 // host → guest: JSON SandboxConfig

	// Port forwarding
	FWD_REQ  byte = 0x20 // host → guest: JSON-encoded ForwardRequest
	FWD_RESP byte = 0x21 // guest → host: JSON-encoded ForwardResponse

	// Sessions
	EXEC_LIST_REQ  byte = 0x30 // host → guest: empty payload
	EXEC_LIST_RESP byte = 0x31 // guest → host: JSON []SessionInfo
	EXEC_KILL      byte = 0x32 // host → guest: JSON {"session_id": "..."}
	SESSION_INFO   byte = 0x33 // guest → host: JSON SessionInfo (sent on create/attach)

	// Activity
	ACTIVITY_REQ  byte = 0x40 // host → guest: empty payload
	ACTIVITY_RESP byte = 0x41 // guest → host: JSON ActivityInfo

	// File operations
	FILE_READ_REQ   byte = 0x50 // host → guest: JSON {"path": "..."}
	FILE_READ_RESP  byte = 0x51 // guest → host: JSON {"size": N, "mode": "0644"} then STDOUT frames, then EXIT
	FILE_WRITE_REQ  byte = 0x52 // host → guest: JSON {"path": "...", "mode": "0644", "size": N} then STDIN frames
	FILE_WRITE_RESP byte = 0x53 // guest → host: JSON {"status": "ok"}
	FILE_STAT_REQ   byte = 0x54 // host → guest: JSON {"path": "..."}
	FILE_STAT_RESP  byte = 0x55 // guest → host: JSON FileInfo
	FILE_LS_REQ     byte = 0x56 // host → guest: JSON {"path": "..."}
	FILE_LS_RESP    byte = 0x57 // guest → host: JSON []FileInfo

	// Systemctl IPC: privileged unit operations are forwarded from the
	// systemctl shim binary (running as caller uid) to PID-1 lohar (running
	// as root) over a Unix domain socket. Spoken on /run/bhatti/systemctl.sock
	// inside the guest — not over vsock, because this is in-guest IPC.
	// Caller uid is established via SO_PEERCRED on the server side, NOT
	// from any client-claimed field; the request payload is just the op +
	// args.
	SYSTEMCTL_REQ  byte = 0x60 // client → lohar: JSON SystemctlRequest
	SYSTEMCTL_RESP byte = 0x61 // lohar → client: JSON SystemctlResponse
)

// Vsock ports
const (
	VsockPortControl = uint32(1024) // exec, shell
	VsockPortForward = uint32(1025) // port forwarding
	VsockPortConfig  = uint32(1026) // boot-time config fetch (guest → host)
)

// MaxFrameSize is the maximum allowed frame size (1 MB).
// The length field value (1 byte type + payload) must not exceed this.
const MaxFrameSize = 1 << 20
