//go:build linux

package main

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	sandboxCAPath     = "usr/local/share/ca-certificates/bhatti-sandbox.crt"
	systemCABundle    = "etc/ssl/certs/ca-certificates.crt"
	sandboxCABundle   = "etc/bhatti/ca-bundle.crt"
	guestCABundlePath = "/etc/bhatti/ca-bundle.crt"
	caMarkerBegin     = "# bhatti-sandbox-ca begin\n"
	caMarkerEnd       = "# bhatti-sandbox-ca end\n"
)

func runCACertUpdate(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func setSandboxCAEnv(env map[string]string, bundle string) {
	for _, name := range []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"} {
		if _, set := env[name]; !set {
			env[name] = bundle
		}
	}
}

func caPaths(root string) (cert, system, bundle string) {
	return filepath.Join(root, sandboxCAPath), filepath.Join(root, systemCABundle), filepath.Join(root, sandboxCABundle)
}

func validatedCA(certPEM string) ([]byte, error) {
	block, rest := pem.Decode([]byte(certPEM))
	if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || strings.TrimSpace(string(rest)) != "" {
		return nil, fmt.Errorf("invalid sandbox CA PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse sandbox CA: %w", err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("sandbox certificate is not a CA")
	}
	return pem.EncodeToMemory(block), nil
}

func readCAFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}

func writeCAFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create %s directory: %w", path, err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

func stripCAMarker(data []byte) []byte {
	for {
		start := bytes.Index(data, []byte(caMarkerBegin))
		if start < 0 || (start > 0 && data[start-1] != '\n') {
			return data
		}
		end := bytes.Index(data[start+len(caMarkerBegin):], []byte(caMarkerEnd))
		if end < 0 {
			return data
		}
		end += start + len(caMarkerBegin) + len(caMarkerEnd)
		data = append(append([]byte(nil), data[:start]...), data[end:]...)
	}
}

func appendCAMarker(data, cert []byte) []byte {
	result := append([]byte(nil), data...)
	if len(result) > 0 && result[len(result)-1] != '\n' {
		result = append(result, '\n')
	}
	result = append(result, caMarkerBegin...)
	result = append(result, cert...)
	result = append(result, caMarkerEnd...)
	return result
}

func appendCA(data, cert []byte) []byte {
	if bytes.Contains(data, cert) {
		return data
	}
	result := append([]byte(nil), data...)
	if len(result) > 0 && result[len(result)-1] != '\n' {
		result = append(result, '\n')
	}
	return append(result, cert...)
}

// installSandboxCA keeps the saved image's old CA out of both bundles before
// trusting the new one; restored images can have belonged to another sandbox.
// A marker means the asynchronous system-store refresh has not yet completed.
func installSandboxCA(root, certPEM string, run func(string, ...string) error, lookPath func(string) (string, error)) (bundle string, refresh func() error, err error) {
	if certPEM == "" {
		return "", nil, removeSandboxCA(root, run, lookPath)
	}
	cert, err := validatedCA(certPEM)
	if err != nil {
		return "", nil, err
	}
	certPath, systemPath, bundlePath := caPaths(root)
	oldCert, err := readCAFile(certPath)
	if err != nil {
		return "", nil, err
	}
	system, err := readCAFile(systemPath)
	if err != nil {
		return "", nil, err
	}
	sameCA := bytes.Equal(oldCert, cert) && bytes.Contains(system, cert)
	markerPending := bytes.Contains(system, []byte(caMarkerBegin))
	if !sameCA {
		if !bytes.Equal(oldCert, cert) {
			if err := writeCAFile(certPath, cert); err != nil {
				return "", nil, err
			}
		}
		cleaned := stripCAMarker(system)
		if len(oldCert) != 0 && !bytes.Equal(oldCert, cert) {
			cleaned = bytes.ReplaceAll(cleaned, oldCert, nil)
		}
		system = appendCAMarker(cleaned, cert)
		if err := writeCAFile(systemPath, system); err != nil {
			return "", nil, err
		}
	}
	if err := writeCAFile(bundlePath, appendCA(system, cert)); err != nil {
		return "", nil, err
	}
	if !sameCA || markerPending {
		if binary, err := lookPath("update-ca-certificates"); err == nil {
			return guestCABundlePath, func() error {
				if err := run(binary); err != nil {
					return fmt.Errorf("update-ca-certificates: %w", err)
				}
				return nil
			}, nil
		}
	}
	return guestCABundlePath, nil, nil
}

func removeSandboxCA(root string, run func(string, ...string) error, lookPath func(string) (string, error)) error {
	certPath, systemPath, bundlePath := caPaths(root)
	oldCert, err := os.ReadFile(certPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", certPath, err)
	}
	system, err := readCAFile(systemPath)
	if err != nil {
		return err
	}
	cleaned := stripCAMarker(system)
	cleaned = bytes.ReplaceAll(cleaned, oldCert, nil)
	if !bytes.Equal(system, cleaned) {
		if err := writeCAFile(systemPath, cleaned); err != nil {
			return err
		}
	}
	if err := os.Remove(certPath); err != nil {
		return fmt.Errorf("remove %s: %w", certPath, err)
	}
	if err := os.Remove(bundlePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", bundlePath, err)
	}
	if binary, err := lookPath("update-ca-certificates"); err == nil {
		if err := run(binary); err != nil {
			return fmt.Errorf("update-ca-certificates after removal: %w", err)
		}
	}
	return nil
}
