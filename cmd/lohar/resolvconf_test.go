//go:build linux

package main

import (
	"strings"
	"testing"
)

// Tests for the resolv.conf rendering used by applyDNS. Order matters:
// the in-cluster DNS responder (10.0.N.1) must come BEFORE any public
// DNS entries so sandbox-name queries hit it first, then NXDOMAIN
// falls through to the public servers for non-sandbox names.
// G1.1 of PLAN-bhatti-v2.md.

func TestBuildResolvConf_InternalFirst(t *testing.T) {
	got := buildResolvConf("10.0.1.1", []string{"1.1.1.1", "8.8.8.8"})

	// Must contain all three nameservers, in the right order.
	lines := nameserverLines(got)
	want := []string{"10.0.1.1", "1.1.1.1", "8.8.8.8"}
	if len(lines) != len(want) {
		t.Fatalf("got %d nameserver lines, want %d: %v", len(lines), len(want), lines)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("ns[%d]: got %q want %q (full output: %q)", i, lines[i], w, got)
		}
	}
}

func TestBuildResolvConf_InternalOnly(t *testing.T) {
	got := buildResolvConf("10.0.5.1", nil)
	lines := nameserverLines(got)
	if len(lines) != 1 || lines[0] != "10.0.5.1" {
		t.Fatalf("got %v, want [10.0.5.1]", lines)
	}
}

func TestBuildResolvConf_InternalHasFastTimeout(t *testing.T) {
	// When the in-cluster responder is the only nameserver, a fast
	// timeout/attempts option means a dead responder fails in ~2s
	// instead of glibc's default ~10s. Pin it so a refactor doesn't
	// silently drop the line and reintroduce the stall.
	got := buildResolvConf("10.0.5.1", nil)
	if !strings.Contains(got, "options timeout:2 attempts:1") {
		t.Errorf("internal resolv.conf missing fast-timeout option:\n%s", got)
	}
}

func TestBuildResolvConf_PublicOnlyHasNoTimeoutOption(t *testing.T) {
	// Degraded mode (responder bind failed) lists public resolvers
	// directly. The fast-timeout option is tied to the in-cluster
	// responder, so it must NOT appear here — public resolvers can be
	// a hair slower and we don't want to give up on them in 2s.
	got := buildResolvConf("", []string{"1.1.1.1", "8.8.8.8"})
	if strings.Contains(got, "options timeout") {
		t.Errorf("public-only resolv.conf should not set a timeout option:\n%s", got)
	}
}

func TestBuildResolvConf_PublicOnly(t *testing.T) {
	// Backwards-compat: a host running an older bhatti daemon won't set
	// DNSInternal, so DNSInternal=="" and we render only public DNS.
	got := buildResolvConf("", []string{"1.1.1.1", "8.8.8.8"})
	lines := nameserverLines(got)
	want := []string{"1.1.1.1", "8.8.8.8"}
	if len(lines) != len(want) {
		t.Fatalf("got %v, want %v", lines, want)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("ns[%d]: got %q want %q", i, lines[i], w)
		}
	}
}

func TestBuildResolvConf_BothEmptyIsEmpty(t *testing.T) {
	// No internal, no public — return "" so applyDNS skips the write.
	// We must NOT clobber an existing system-installed resolv.conf with
	// an empty file in this case.
	if got := buildResolvConf("", nil); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestBuildResolvConf_IncludesCommentForInternal(t *testing.T) {
	// The comment line documents what 10.0.N.1 is. Good for a sysadmin
	// who SSHes into a sandbox and runs `cat /etc/resolv.conf`. The
	// nameserver line is what actually matters; the comment is for
	// humans. A regression that removes the comment is cosmetic, not
	// semantic — but we pin it anyway so anyone changing the format
	// makes the change deliberately.
	got := buildResolvConf("10.0.1.1", nil)
	if !strings.HasPrefix(got, "# bhatti ") {
		t.Fatalf("expected leading comment, got %q", got[:min(len(got), 50)])
	}
}

func TestBuildResolvConf_PublicOnlyHasNoComment(t *testing.T) {
	// When there's no in-cluster DNS, we shouldn't write the bhatti
	// comment header — it would be misleading.
	got := buildResolvConf("", []string{"1.1.1.1"})
	if strings.Contains(got, "# bhatti") {
		t.Fatalf("public-only output should not mention bhatti: %q", got)
	}
}

// nameserverLines extracts the IPs from each "nameserver X" line in
// resolv-conf-formatted text, preserving order.
func nameserverLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "nameserver ") {
			continue
		}
		out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "nameserver ")))
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
