//go:build linux

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sandboxCATestCert(t *testing.T, isCA bool) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{}
	template.SerialNumber = big.NewInt(time.Now().UnixNano())
	template.Subject = pkix.Name{CommonName: "sandbox test CA"}
	template.NotBefore = time.Now().Add(-time.Hour)
	template.NotAfter = time.Now().Add(time.Hour)
	template.BasicConstraintsValid = true
	template.IsCA = isCA
	if isCA {
		template.KeyUsage = x509.KeyUsageCertSign
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func sandboxCANoUpdate(string) (string, error) { return "", errors.New("missing") }

func sandboxCARead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sandboxCASeed(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxCAFallbackBundle(t *testing.T) {
	root, ca := t.TempDir(), sandboxCATestCert(t, true)
	cert, system, bundle := caPaths(root)
	sandboxCASeed(t, system, "system roots\n")
	guestPath, refresh, err := installSandboxCA(root, ca, func(string, ...string) error {
		t.Fatal("unexpected update")
		return nil
	}, sandboxCANoUpdate)
	if err != nil || guestPath != guestCABundlePath {
		t.Fatalf("install: bundle=%q err=%v", guestPath, err)
	}
	if refresh != nil {
		t.Fatal("updater absent but refresh returned")
	}
	if got := sandboxCARead(t, cert); got != ca {
		t.Fatal("CA source differs")
	}
	got := sandboxCARead(t, system)
	if got != "system roots\n"+caMarkerBegin+ca+caMarkerEnd {
		t.Fatalf("system bundle missing marker or original roots: %q", got)
	}
	if got := sandboxCARead(t, bundle); got != "system roots\n"+caMarkerBegin+ca+caMarkerEnd {
		t.Fatalf("env bundle missing system roots or CA: %q", got)
	}
}

func TestSandboxCAReplacesDifferentCA(t *testing.T) {
	root, oldCA, newCA := t.TempDir(), sandboxCATestCert(t, true), sandboxCATestCert(t, true)
	_, system, bundle := caPaths(root)
	sandboxCASeed(t, system, "system roots\n")
	for _, ca := range []string{oldCA, newCA} {
		if _, refresh, err := installSandboxCA(root, ca, func(string, ...string) error {
			t.Fatal("unexpected update")
			return nil
		}, sandboxCANoUpdate); err != nil {
			t.Fatal(err)
		} else if refresh != nil {
			t.Fatal("updater absent but refresh returned")
		}
	}
	for _, path := range []string{system, bundle} {
		got := sandboxCARead(t, path)
		if strings.Contains(got, oldCA) || strings.Count(got, newCA) != 1 || strings.Count(got, caMarkerBegin) != 1 || !strings.Contains(got, "system roots") {
			t.Fatalf("old CA retained or new CA duplicated in %s: %q", path, got)
		}
	}
}

func TestSandboxCAUpdateCertificates(t *testing.T) {
	root, ca := t.TempDir(), sandboxCATestCert(t, true)
	_, system, bundle := caPaths(root)
	sandboxCASeed(t, system, "system roots\n")
	calls := 0
	look := func(string) (string, error) { return "/usr/sbin/update-ca-certificates", nil }
	run := func(name string, args ...string) error {
		if name != "/usr/sbin/update-ca-certificates" {
			t.Fatalf("binary: %s", name)
		}
		calls++
		return os.WriteFile(system, []byte("system roots\n"+ca), 0644)
	}
	guestPath, refresh, err := installSandboxCA(root, ca, run, look)
	if err != nil || guestPath != guestCABundlePath {
		t.Fatalf("install: bundle=%q err=%v", guestPath, err)
	}
	if calls != 0 || refresh == nil {
		t.Fatalf("refresh ran during install or was not returned: calls=%d refresh=%v", calls, refresh != nil)
	}
	for _, path := range []string{system, bundle} {
		got := sandboxCARead(t, path)
		if !strings.Contains(got, caMarkerBegin+ca+caMarkerEnd) || !strings.Contains(got, "system roots") {
			t.Fatalf("CA not trusted before refresh at %s: %q", path, got)
		}
	}
	if err := refresh(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("update calls after refresh: %d", calls)
	}
	if got := sandboxCARead(t, system); strings.Contains(got, caMarkerBegin) || strings.Count(got, ca) != 1 {
		t.Fatalf("refresh failed to regenerate system store: %q", got)
	}
}

func TestSandboxCAPendingRefresh(t *testing.T) {
	root, ca := t.TempDir(), sandboxCATestCert(t, true)
	cert, system, bundle := caPaths(root)
	sandboxCASeed(t, system, "system roots\n")
	calls := 0
	run := func(string, ...string) error {
		calls++
		return os.WriteFile(system, []byte("system roots\n"+ca), 0644)
	}
	look := func(string) (string, error) { return "update-ca-certificates", nil }
	if _, refresh, err := installSandboxCA(root, ca, run, look); err != nil || refresh == nil {
		t.Fatalf("first install: refresh=%v err=%v", refresh != nil, err)
	}
	originalSystem := sandboxCARead(t, system)
	oldTime := time.Unix(1000, 0)
	for _, path := range []string{cert, system} {
		if err := os.Chtimes(path, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(bundle); err != nil {
		t.Fatal(err)
	}
	_, refresh, err := installSandboxCA(root, ca, run, look)
	if err != nil || refresh == nil || calls != 0 {
		t.Fatalf("pending refresh ran inline or was lost: refresh=%v calls=%d err=%v", refresh != nil, calls, err)
	}
	for _, path := range []string{cert, system} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(oldTime) {
			t.Fatalf("sync install rewrote %s: time=%v", path, info.ModTime())
		}
	}
	if got := sandboxCARead(t, system); got != originalSystem {
		t.Fatal("pending marker was changed")
	}
	if got := sandboxCARead(t, bundle); !strings.Contains(got, ca) {
		t.Fatal("env bundle was not rewritten")
	}
	if err := refresh(); err != nil || calls != 1 {
		t.Fatalf("pending refresh: calls=%d err=%v", calls, err)
	}
	_, refresh, err = installSandboxCA(root, ca, run, look)
	if err != nil || refresh != nil {
		t.Fatalf("refreshed CA still pending: refresh=%v err=%v", refresh != nil, err)
	}
}

func TestSandboxCAIdempotent(t *testing.T) {
	root, ca := t.TempDir(), sandboxCATestCert(t, true)
	_, system, bundle := caPaths(root)
	sandboxCASeed(t, system, "system roots\n")
	calls := 0
	run := func(string, ...string) error {
		calls++
		return os.WriteFile(system, []byte("system roots\n"+ca), 0644)
	}
	look := func(string) (string, error) { return "update-ca-certificates", nil }
	_, refresh, err := installSandboxCA(root, ca, run, look)
	if err != nil || refresh == nil {
		t.Fatalf("first install: refresh=%v err=%v", refresh != nil, err)
	}
	if err := refresh(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bundle); err != nil {
		t.Fatal(err)
	}
	_, refresh, err = installSandboxCA(root, ca, run, look)
	if err != nil || refresh != nil {
		t.Fatalf("fast path returned refresh=%v err=%v", refresh != nil, err)
	}
	if calls != 1 {
		t.Fatalf("idempotent install called update %d times", calls)
	}
	if !strings.Contains(sandboxCARead(t, bundle), ca) {
		t.Fatal("fast path failed to rewrite env bundle")
	}
}

func TestSandboxCARemovesStaleInstall(t *testing.T) {
	root, ca := t.TempDir(), sandboxCATestCert(t, true)
	cert, system, bundle := caPaths(root)
	sandboxCASeed(t, system, "system roots\n")
	if _, _, err := installSandboxCA(root, ca, func(string, ...string) error { return nil }, sandboxCANoUpdate); err != nil {
		t.Fatal(err)
	}
	calls := 0
	look := func(string) (string, error) { return "update-ca-certificates", nil }
	if err := removeSandboxCA(root, func(string, ...string) error {
		calls++
		return nil
	}, look); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("cleanup update calls: %d", calls)
	}
	for _, path := range []string{cert, bundle} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists: %v", path, err)
		}
	}
	if got := sandboxCARead(t, system); got != "system roots\n" {
		t.Fatalf("stale CA remained: %q", got)
	}
}

func TestSandboxCANoStaleInstall(t *testing.T) {
	root := t.TempDir()
	calls := 0
	if err := removeSandboxCA(root, func(string, ...string) error {
		calls++
		return nil
	}, func(string) (string, error) {
		calls++
		return "update-ca-certificates", nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("clean image caused %d update calls", calls)
	}
	for _, path := range []string{sandboxCAPath, systemCABundle, sandboxCABundle} {
		if _, err := os.Stat(filepath.Join(root, path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("created %s: %v", path, err)
		}
	}
}

func TestSandboxCAEnvPreservesUserValue(t *testing.T) {
	env := map[string]string{"SSL_CERT_FILE": "/custom/roots.crt"}
	setSandboxCAEnv(env, guestCABundlePath)
	if env["SSL_CERT_FILE"] != "/custom/roots.crt" {
		t.Fatal("overwrote user SSL_CERT_FILE")
	}
	for _, key := range []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"} {
		if env[key] != guestCABundlePath {
			t.Fatalf("missing %s: %q", key, env[key])
		}
	}
	old := configEnv
	configEnv = env
	t.Cleanup(func() { configEnv = old })
	merged := strings.Join(buildEnv(nil), "\n")
	for key, value := range env {
		if !strings.Contains(merged, key+"="+value) {
			t.Fatalf("exec env missing %s", key)
		}
	}
}

func TestSandboxCARejectsInvalidCertificate(t *testing.T) {
	for _, candidate := range []string{"garbage", sandboxCATestCert(t, false)} {
		root := t.TempDir()
		calls := 0
		_, _, err := installSandboxCA(root, candidate, func(string, ...string) error {
			calls++
			return nil
		}, func(string) (string, error) {
			calls++
			return "update-ca-certificates", nil
		})
		if err == nil {
			t.Fatalf("accepted invalid certificate: %q", candidate)
		}
		if calls != 0 {
			t.Fatal("invalid certificate invoked update")
		}
		for _, path := range []string{sandboxCAPath, systemCABundle, sandboxCABundle} {
			if _, err := os.Stat(filepath.Join(root, path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid certificate created %s: %v", path, err)
			}
		}
	}
}

func TestSandboxCAFailedUpdateFallsBack(t *testing.T) {
	root, ca := t.TempDir(), sandboxCATestCert(t, true)
	_, system, bundle := caPaths(root)
	sandboxCASeed(t, system, "system roots\n")
	_, refresh, err := installSandboxCA(root, ca, func(string, ...string) error { return errors.New("failed") }, func(string) (string, error) { return "update-ca-certificates", nil })
	if err != nil || refresh == nil {
		t.Fatalf("failed-update install: refresh=%v err=%v", refresh != nil, err)
	}
	if err := refresh(); err == nil {
		t.Fatal("refresh failure was not reported")
	}
	if !bytes.Contains([]byte(sandboxCARead(t, system)), []byte(caMarkerBegin+ca+caMarkerEnd)) || !strings.Contains(sandboxCARead(t, bundle), ca) {
		t.Fatal("failed update did not fall back")
	}
}
