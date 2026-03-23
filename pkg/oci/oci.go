// Package oci handles pulling OCI/Docker images, flattening layers to a
// directory tree, injecting the bhatti guest agent (lohar), and creating
// ext4 rootfs images suitable for Firecracker microVMs.
package oci

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Config holds extracted OCI image configuration metadata.
type Config struct {
	Env        map[string]string `json:"env,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Cmd        []string          `json:"cmd,omitempty"`
	User       string            `json:"user,omitempty"`
	TotalSize  int64             `json:"total_size"` // flattened size in bytes
}

// Option configures PullAndConvert behavior.
type Option func(*pullOptions)

type pullOptions struct {
	auth     authn.Authenticator
	platform v1.Platform
	progress func(string)
}

// WithAuth sets basic auth credentials for the registry.
func WithAuth(user, password string) Option {
	return func(o *pullOptions) {
		o.auth = &authn.Basic{Username: user, Password: password}
	}
}

// WithPlatform overrides the target platform (defaults to linux/amd64 on amd64 hosts).
func WithPlatform(os, arch string) Option {
	return func(o *pullOptions) {
		o.platform = v1.Platform{OS: os, Architecture: arch}
	}
}

// WithProgress sets a callback for progress updates.
func WithProgress(fn func(string)) Option {
	return func(o *pullOptions) {
		o.progress = fn
	}
}

// PullAndConvert pulls an OCI image from a registry, flattens it to an
// ext4 rootfs image, injects the lohar agent, and returns the extracted
// OCI config for storage.
//
// The pull uses streaming (remote.Image) to avoid loading all layers into
// memory — critical for large images (CUDA: 5-10GB).
func PullAndConvert(ctx context.Context, ref, outputPath, loharPath string, opts ...Option) (*Config, error) {
	o := &pullOptions{
		auth:     authn.Anonymous,
		platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH},
		progress: func(string) {},
	}
	for _, opt := range opts {
		opt(o)
	}

	// 1. Resolve image reference
	imgRef, err := name.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("parse ref %q: %w", ref, err)
	}

	o.progress("resolving image")

	// 2. Pull image descriptor (streaming — layers are fetched on demand)
	remoteOpts := []remote.Option{
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(o.platform),
		remote.WithContext(ctx),
	}
	if o.auth != authn.Anonymous {
		remoteOpts = append(remoteOpts, remote.WithAuth(o.auth))
	}

	desc, err := remote.Get(imgRef, remoteOpts...)
	if err != nil {
		return nil, fmt.Errorf("pull %q: %w", ref, err)
	}

	img, err := desc.Image()
	if err != nil {
		return nil, fmt.Errorf("image %q: %w", ref, err)
	}

	// 3. Extract OCI config
	cfgFile, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("config %q: %w", ref, err)
	}
	config := extractConfig(cfgFile)

	// 4. Flatten layers to temp directory
	o.progress("flattening layers")
	tmpDir, err := os.MkdirTemp("", "bhatti-oci-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("layers %q: %w", ref, err)
	}

	for i, layer := range layers {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		o.progress(fmt.Sprintf("extracting layer %d/%d", i+1, len(layers)))
		if err := extractLayer(layer, tmpDir); err != nil {
			return nil, fmt.Errorf("extract layer %d: %w", i, err)
		}
	}

	// 5. Inject bhatti components
	o.progress("injecting lohar")
	if err := injectLohar(tmpDir, loharPath); err != nil {
		return nil, fmt.Errorf("inject lohar: %w", err)
	}

	// 6. Validate compatibility
	if warnings := validateImage(tmpDir); len(warnings) > 0 {
		for _, w := range warnings {
			slog.Warn("oci image warning", "ref", ref, "issue", w)
		}
	}

	// 7. Create ext4 image
	o.progress("creating ext4 image")
	if err := createExt4FromDir(tmpDir, outputPath); err != nil {
		return nil, fmt.Errorf("create ext4: %w", err)
	}

	// Measure final size
	if fi, err := os.Stat(outputPath); err == nil {
		config.TotalSize = fi.Size()
	}

	return config, nil
}

func extractConfig(cfg *v1.ConfigFile) *Config {
	c := &Config{
		Env:        make(map[string]string),
		WorkingDir: cfg.Config.WorkingDir,
		User:       cfg.Config.User,
	}
	if len(cfg.Config.Cmd) > 0 {
		c.Cmd = cfg.Config.Cmd
	}
	if len(cfg.Config.Entrypoint) > 0 && len(c.Cmd) == 0 {
		c.Cmd = cfg.Config.Entrypoint
	}
	for _, e := range cfg.Config.Env {
		if k, v, ok := splitEnv(e); ok {
			c.Env[k] = v
		}
	}
	return c
}

func splitEnv(s string) (string, string, bool) {
	for i, c := range s {
		if c == '=' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}
