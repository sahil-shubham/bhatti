//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/sahilshubham/bhatti/pkg/agent/proto"
)

type frameMsg struct {
	msgType byte
	payload []byte
}

func handlePipedExec(conn net.Conn, req proto.ExecRequest) {
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Env = buildEnv(req.Env)
	if req.Cwd != nil {
		cmd.Dir = *req.Cwd
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("stdin pipe: %v", err)))
		return
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("stdout pipe: %v", err)))
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("stderr pipe: %v", err)))
		return
	}

	if err := cmd.Start(); err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("start: %v", err)))
		return
	}

	// Serialize all frame writes through a channel.
	tx := make(chan frameMsg, 64)
	var writerWg sync.WaitGroup
	writerWg.Add(1)
	go func() {
		defer writerWg.Done()
		for msg := range tx {
			proto.WriteFrame(conn, msg.msgType, msg.payload)
		}
	}()

	// stdout → STDOUT frames
	var ioWg sync.WaitGroup
	ioWg.Add(2)
	go func() {
		defer ioWg.Done()
		buf := make([]byte, 8192)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				tx <- frameMsg{proto.STDOUT, data}
			}
			if err != nil {
				return
			}
		}
	}()

	// stderr → STDERR frames
	go func() {
		defer ioWg.Done()
		buf := make([]byte, 8192)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				tx <- frameMsg{proto.STDERR, data}
			}
			if err != nil {
				return
			}
		}
	}()

	// conn → stdin + KILL handling
	go func() {
		defer stdinPipe.Close()
		for {
			msgType, payload, err := proto.ReadFrame(conn)
			if err != nil {
				return
			}
			switch msgType {
			case proto.STDIN:
				stdinPipe.Write(payload)
			case proto.KILL:
				cmd.Process.Signal(syscall.SIGTERM)
				return
			}
		}
	}()

	// Wait for stdout/stderr to drain, then wait for child.
	ioWg.Wait()
	exitCode := exitCodeFromErr(cmd.Wait())

	syscall.Sync()
	exit := proto.ExitPayload(int32(exitCode))
	tx <- frameMsg{proto.EXIT, exit[:]}
	close(tx)
	writerWg.Wait()
}

func exitCodeFromErr(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		status := exitErr.Sys().(syscall.WaitStatus)
		if status.Signaled() {
			return 128 + int(status.Signal())
		}
		return status.ExitStatus()
	}
	return 1
}

func buildEnv(env map[string]string) []string {
	defaults := map[string]string{
		"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TERM": "xterm-256color",
		"HOME": "/root",
		"LANG": "en_US.UTF-8",
	}
	// Merge config drive env vars (secrets, etc.)
	for k, v := range configEnv {
		defaults[k] = v
	}
	// Per-request env overrides everything
	for k, v := range env {
		defaults[k] = v
	}
	result := make([]string, 0, len(defaults))
	for k, v := range defaults {
		result = append(result, k+"="+v)
	}
	return result
}

// logf logs to stderr with a prefix.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "lohar: "+format+"\n", args...)
}
