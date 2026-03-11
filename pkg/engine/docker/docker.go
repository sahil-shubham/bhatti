package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/sahilshubham/bhatti/pkg/engine"
)

const labelPrefix = "bhatti."

// Engine implements engine.Engine using Docker.
type Engine struct {
	cli *client.Client
}

// New creates a Docker engine using the default Docker socket.
func New() (*Engine, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Engine{cli: cli}, nil
}

func (e *Engine) Create(ctx context.Context, spec engine.SandboxSpec) (engine.SandboxInfo, error) {
	// Pull image if not present
	_, _, err := e.cli.ImageInspectWithRaw(ctx, spec.Image)
	if err != nil {
		rc, pullErr := e.cli.ImagePull(ctx, spec.Image, image.PullOptions{})
		if pullErr != nil {
			return engine.SandboxInfo{}, fmt.Errorf("pull image %s: %w", spec.Image, pullErr)
		}
		io.Copy(io.Discard, rc)
		rc.Close()
	}

	// Build env vars
	var envList []string
	for k, v := range spec.Env {
		envList = append(envList, k+"="+v)
	}

	// Labels
	labels := map[string]string{
		labelPrefix + "managed": "true",
		labelPrefix + "name":    spec.Name,
	}
	for k, v := range spec.Labels {
		labels[labelPrefix+"label."+k] = v
	}

	// Resource limits
	var resources container.Resources
	if spec.CPUs > 0 {
		resources.NanoCPUs = int64(spec.CPUs * 1e9)
	}
	if spec.MemoryMB > 0 {
		resources.Memory = int64(spec.MemoryMB) * 1024 * 1024
	}

	cfg := &container.Config{
		Image:  spec.Image,
		Env:    envList,
		Labels: labels,
		Tty:    true,
		// Keep container running
		Cmd: []string{"/bin/sh", "-c", "exec sleep infinity"},
	}

	// If userdata is set, use it as the command
	if spec.UserData != "" {
		cfg.Cmd = []string{"/bin/sh", "-c", spec.UserData}
	}

	hostCfg := &container.HostConfig{
		Resources: resources,
	}

	resp, err := e.cli.ContainerCreate(ctx, cfg, hostCfg, &network.NetworkingConfig{}, nil, spec.Name)
	if err != nil {
		return engine.SandboxInfo{}, fmt.Errorf("create container: %w", err)
	}

	if err := e.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Clean up on failed start
		e.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return engine.SandboxInfo{}, fmt.Errorf("start container: %w", err)
	}

	return e.Status(ctx, resp.ID)
}

func (e *Engine) Destroy(ctx context.Context, id string) error {
	return e.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
}

func (e *Engine) Stop(ctx context.Context, id string) error {
	timeout := 10
	return e.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout})
}

func (e *Engine) Start(ctx context.Context, id string) error {
	return e.cli.ContainerStart(ctx, id, container.StartOptions{})
}

func (e *Engine) Status(ctx context.Context, id string) (engine.SandboxInfo, error) {
	info, err := e.cli.ContainerInspect(ctx, id)
	if err != nil {
		return engine.SandboxInfo{}, fmt.Errorf("inspect container: %w", err)
	}

	status := "unknown"
	if info.State != nil {
		switch {
		case info.State.Running:
			status = "running"
		case info.State.Paused:
			status = "paused"
		default:
			status = "stopped"
		}
	}

	ip := ""
	if info.NetworkSettings != nil && info.NetworkSettings.DefaultNetworkSettings.IPAddress != "" {
		ip = info.NetworkSettings.DefaultNetworkSettings.IPAddress
	}

	name := strings.TrimPrefix(info.Name, "/")

	return engine.SandboxInfo{
		ID:       info.ID[:12],
		Name:     name,
		Status:   status,
		IP:       ip,
		EngineID: info.ID,
	}, nil
}

func (e *Engine) List(ctx context.Context) ([]engine.SandboxInfo, error) {
	f := filters.NewArgs()
	f.Add("label", labelPrefix+"managed=true")

	containers, err := e.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: f,
	})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	var out []engine.SandboxInfo
	for _, c := range containers {
		status := "unknown"
		switch {
		case strings.HasPrefix(c.State, "running"):
			status = "running"
		case strings.HasPrefix(c.State, "exited"), strings.HasPrefix(c.State, "dead"):
			status = "stopped"
		case strings.HasPrefix(c.State, "paused"):
			status = "paused"
		}

		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}

		ip := ""
		if c.NetworkSettings != nil {
			for _, n := range c.NetworkSettings.Networks {
				if n.IPAddress != "" {
					ip = n.IPAddress
					break
				}
			}
		}

		out = append(out, engine.SandboxInfo{
			ID:       c.ID[:12],
			Name:     name,
			Status:   status,
			IP:       ip,
			EngineID: c.ID,
		})
	}
	return out, nil
}

func (e *Engine) Exec(ctx context.Context, id string, cmd []string) (engine.ExecResult, error) {
	execCfg := types.ExecConfig{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}
	exec, err := e.cli.ContainerExecCreate(ctx, id, execCfg)
	if err != nil {
		return engine.ExecResult{}, fmt.Errorf("exec create: %w", err)
	}

	resp, err := e.cli.ContainerExecAttach(ctx, exec.ID, types.ExecStartCheck{})
	if err != nil {
		return engine.ExecResult{}, fmt.Errorf("exec attach: %w", err)
	}
	defer resp.Close()

	var stdout, stderr bytes.Buffer
	_, err = stdcopy.StdCopy(&stdout, &stderr, resp.Reader)
	if err != nil {
		return engine.ExecResult{}, fmt.Errorf("exec read: %w", err)
	}

	inspect, err := e.cli.ContainerExecInspect(ctx, exec.ID)
	if err != nil {
		return engine.ExecResult{}, fmt.Errorf("exec inspect: %w", err)
	}

	return engine.ExecResult{
		ExitCode: inspect.ExitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

// termConn wraps a Docker exec attach for TTY usage.
type termConn struct {
	execID string
	resp   types.HijackedResponse
	cli    *client.Client
}

func (t *termConn) Read(p []byte) (int, error)  { return t.resp.Reader.Read(p) }
func (t *termConn) Write(p []byte) (int, error) { return t.resp.Conn.Write(p) }
func (t *termConn) Close() error                { t.resp.Close(); return nil }
func (t *termConn) Resize(rows, cols int) error {
	return t.cli.ContainerExecResize(context.Background(), t.execID, container.ResizeOptions{
		Height: uint(rows),
		Width:  uint(cols),
	})
}

func (e *Engine) Shell(ctx context.Context, id string) (engine.TerminalConn, error) {
	// Inspect container to determine user and shell
	info, err := e.cli.ContainerInspect(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("inspect for shell: %w", err)
	}

	user := info.Config.User
	shell := "/bin/sh"
	workDir := "/"

	if user != "" && user != "root" {
		// Try the user's login shell via getent
		result, err := e.Exec(ctx, id, []string{"getent", "passwd", user})
		if err == nil && result.ExitCode == 0 {
			// getent output: user:x:uid:gid:gecos:home:shell
			parts := strings.SplitN(strings.TrimSpace(result.Stdout), ":", 7)
			if len(parts) == 7 {
				if parts[5] != "" {
					workDir = parts[5]
				}
				if parts[6] != "" {
					shell = parts[6]
				}
			}
		}
	}

	execCfg := types.ExecConfig{
		Cmd:          []string{shell, "-li"},
		User:         user,
		WorkingDir:   workDir,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		Env:          []string{"TERM=xterm-256color"},
	}
	exec, err := e.cli.ContainerExecCreate(ctx, id, execCfg)
	if err != nil {
		return nil, fmt.Errorf("shell exec create: %w", err)
	}

	resp, err := e.cli.ContainerExecAttach(ctx, exec.ID, types.ExecStartCheck{Tty: true})
	if err != nil {
		return nil, fmt.Errorf("shell exec attach: %w", err)
	}

	return &termConn{
		execID: exec.ID,
		resp:   resp,
		cli:    e.cli,
	}, nil
}
