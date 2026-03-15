//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/sahilshubham/bhatti/pkg/agent/proto"
)

// TCP port constants reuse the vsock port numbers for simplicity.
// The agent listens on both vsock AND TCP on the same port numbers.

func main() {
	if os.Getenv("BHATTI_AGENT_TEST") == "1" {
		runTestMode()
		return
	}

	// --- PID 1 init ---

	// Set PATH for the agent process itself. As PID 1, we inherit no
	// environment. exec.Command uses LookPath which checks our PATH.
	os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	os.Setenv("HOME", "/root")

	// Mount essential filesystems.
	mustMount("proc", "/proc", "proc", 0, "")
	mustMount("sysfs", "/sys", "sysfs", 0, "")
	mustMount("devtmpfs", "/dev", "devtmpfs", 0, "")
	os.MkdirAll("/dev/pts", 0755)
	mustMount("devpts", "/dev/pts", "devpts", 0, "newinstance,ptmxmode=0666")
	mustMount("tmpfs", "/tmp", "tmpfs", 0, "")
	mustMount("tmpfs", "/run", "tmpfs", 0, "")

	syscall.Sethostname([]byte("bhatti"))

	bringUpInterface("lo")
	setupNetworking()
	ensureResolvConf()
	installSignalHandlers()

	// Listen on vsock (works for cold boot, broken after snapshot/restore).
	lnControl, err := listenVsock(proto.VsockPortControl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bhatti-agent: vsock control: %v\n", err)
		// Non-fatal: TCP listeners below are the primary channel.
	} else {
		go acceptLoop(lnControl, handleControlConnection)
	}
	lnForward, err := listenVsock(proto.VsockPortForward)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bhatti-agent: vsock forward: %v\n", err)
	} else {
		go acceptLoop(lnForward, handleForwardConnection)
	}

	// Listen on TCP (survives snapshot/restore since virtio-net is reliable).
	// The guest IP is configured by the kernel's ip= cmdline parameter
	// before init runs, so the interface is already up.
	tcpControl, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", proto.VsockPortControl))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bhatti-agent: tcp control: %v\n", err)
	} else {
		go acceptLoop(tcpControl, handleControlConnection)
	}
	tcpForward, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", proto.VsockPortForward))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bhatti-agent: tcp forward: %v\n", err)
	} else {
		go acceptLoop(tcpForward, handleForwardConnection)
	}

	fmt.Fprintln(os.Stderr, "bhatti-agent: ready")

	// PID 1 must never exit.
	select {}
}

func mustMount(source, target, fstype string, flags uintptr, data string) {
	os.MkdirAll(target, 0755)
	if err := syscall.Mount(source, target, fstype, flags, data); err != nil {
		fmt.Fprintf(os.Stderr, "bhatti-agent: mount %s on %s: %v\n", source, target, err)
	}
}

func ensureResolvConf() {
	const path = "/etc/resolv.conf"
	// Remove any broken symlink (e.g. from systemd-resolved stub)
	os.Remove(path)
	if err := os.WriteFile(path, []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "bhatti-agent: write resolv.conf: %v\n", err)
	}
}

func acceptLoop(ln net.Listener, handler func(net.Conn)) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handler(conn)
	}
}

func installSignalHandlers() {
	// Note: we do NOT install a SIGCHLD handler. Go's runtime manages
	// SIGCHLD for processes started via exec.Command. A manual Wait4(-1)
	// reaper would race with cmd.Wait() and corrupt exit codes.
	// Orphan zombies (from grandchild processes) are acceptable for now.

	// SIGTERM/SIGINT: clean shutdown.
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigterm
		syscall.Sync()
		syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
	}()
}
