package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func grantFixture(id, user, secret, sandbox, placeholder string, created time.Time) SecretGrant {
	return SecretGrant{
		ID: id, UserID: user, SecretName: secret, SandboxID: sandbox,
		Hosts: []string{"api.github.com", "*.example.com"}, Placeholder: placeholder,
		CreatedAt: created,
	}
}

func putGrant(t *testing.T, s *Store, g SecretGrant) {
	t.Helper()
	if err := s.CreateSecretGrant(g); err != nil {
		t.Fatal(err)
	}
}

func TestSecretGrantCreateAndLookup(t *testing.T) {
	s := testStore(t)
	when := time.Date(2026, 10, 4, 12, 30, 0, 0, time.FixedZone("west", -7*3600))
	expires := when.Add(time.Hour)
	g := grantFixture("g1", "alice", "GH_TOKEN", "sb1", "bhatti_sec_123", when)
	g.ExpiresAt = &expires
	putGrant(t, s, g)
	got, err := s.GetSecretGrantByPlaceholder(g.Placeholder)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != g.ID || got.UserID != g.UserID || got.SecretName != g.SecretName || got.SandboxID != g.SandboxID || got.Placeholder != g.Placeholder || !reflect.DeepEqual(got.Hosts, g.Hosts) {
		t.Fatalf("grant mismatch: %+v", got)
	}
	if !got.CreatedAt.Equal(when) || got.CreatedAt.Location() != time.UTC || got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) || got.ExpiresAt.Location() != time.UTC || got.RevokedAt != nil {
		t.Fatalf("times not UTC or nullable: %+v", got)
	}
	if _, err := s.GetSecretGrantByPlaceholder("absent"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("absent placeholder: %v", err)
	}
}

func TestSecretGrantOwnership(t *testing.T) {
	s := testStore(t)
	g := grantFixture("g1", "alice", "GH_TOKEN", "sb1", "placeholder1", time.Now())
	putGrant(t, s, g)
	if _, err := s.GetSecretGrant("bob", g.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("other user can read grant: %v", err)
	}
	if _, err := s.GetSecretGrant("alice", "absent"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("absent grant: %v", err)
	}
	got, err := s.GetSecretGrant("alice", g.ID)
	if err != nil || got.ID != g.ID {
		t.Fatalf("owner cannot read grant: %+v, %v", got, err)
	}
}

func TestSecretGrantUniquePlaceholder(t *testing.T) {
	s := testStore(t)
	g := grantFixture("g1", "alice", "GH_TOKEN", "sb1", "placeholder1", time.Now())
	putGrant(t, s, g)
	duplicate := grantFixture("g2", "bob", "TOKEN", "sb2", g.Placeholder, time.Now())
	if err := s.CreateSecretGrant(duplicate); err == nil {
		t.Fatal("duplicate placeholder accepted")
	}
}

func TestSecretGrantLive(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	g := &SecretGrant{}
	if !g.Live(now) {
		t.Fatal("grant without expiry must be live")
	}
	expires := now.Add(time.Second)
	g.ExpiresAt = &expires
	if !g.Live(now) {
		t.Fatal("future expiry should be live")
	}
	if g.Live(expires) || g.Live(now.Add(2*time.Second)) {
		t.Fatal("grant at or after expiry should not be live")
	}
	g.ExpiresAt = nil
	g.RevokedAt = &now
	if g.Live(now.Add(-time.Second)) {
		t.Fatal("revoked grant should never be live")
	}
}

func TestSecretGrantRevocation(t *testing.T) {
	s := testStore(t)
	putGrant(t, s, grantFixture("g1", "alice", "GH_TOKEN", "sb1", "placeholder1", time.Now()))
	first := time.Date(2026, 10, 4, 12, 0, 0, 0, time.FixedZone("east", 3*3600))
	if err := s.RevokeSecretGrant("bob", "g1", first); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("wrong user revoke: %v", err)
	}
	if err := s.RevokeSecretGrant("alice", "absent", first); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing grant revoke: %v", err)
	}
	if err := s.RevokeSecretGrant("alice", "g1", first); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSecretGrant("alice", "g1", first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSecretGrant("alice", "g1")
	if err != nil || got.RevokedAt == nil || !got.RevokedAt.Equal(first) || got.RevokedAt.Location() != time.UTC {
		t.Fatalf("first revocation not retained in UTC: %+v, %v", got, err)
	}
}

func TestSecretGrantLists(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	putGrant(t, s, grantFixture("old", "alice", "GH", "sb1", "p1", start))
	putGrant(t, s, grantFixture("other", "bob", "GH", "sb1", "p2", start.Add(time.Minute)))
	putGrant(t, s, grantFixture("new", "alice", "API", "sb2", "p3", start.Add(2*time.Minute)))
	grants, err := s.ListSecretGrants("alice")
	if err != nil || len(grants) != 2 || grants[0].ID != "new" || grants[1].ID != "old" {
		t.Fatalf("user's newest-first grants: %+v, %v", grants, err)
	}
	grants, err = s.ListSandboxSecretGrants("sb1")
	if err != nil || len(grants) != 2 || grants[0].ID != "other" || grants[1].ID != "old" {
		t.Fatalf("sandbox grants: %+v, %v", grants, err)
	}
}

func TestSecretGrantDeletionScope(t *testing.T) {
	s := testStore(t)
	start := time.Now().UTC()
	for _, g := range []SecretGrant{
		grantFixture("a1", "alice", "GH", "sb1", "p1", start),
		grantFixture("a2", "alice", "GH", "sb2", "p2", start),
		grantFixture("b1", "bob", "GH", "sb2", "p3", start),
		grantFixture("a3", "alice", "API", "sb2", "p4", start),
	} {
		putGrant(t, s, g)
	}
	if err := s.DeleteSandboxSecretGrants("sb1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSecretGrant("alice", "a1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("sandbox grant survived: %v", err)
	}
	if err := s.DeleteSecretGrantsForSecret("alice", "GH"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSecretGrant("alice", "a2"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("secret grant survived: %v", err)
	}
	for _, tc := range []struct{ user, id string }{{"bob", "b1"}, {"alice", "a3"}} {
		if _, err := s.GetSecretGrant(tc.user, tc.id); err != nil {
			t.Fatalf("unrelated grant %s deleted: %v", tc.id, err)
		}
	}
	if err := s.DeleteSandboxSecretGrants("sb2"); err != nil {
		t.Fatal(err)
	}
	grants, err := s.ListSandboxSecretGrants("sb2")
	if err != nil || len(grants) != 0 {
		t.Fatalf("sandbox grants survived deletion: %+v, %v", grants, err)
	}
}

func TestSandboxCAPutGetReplaceDelete(t *testing.T) {
	s := testStore(t)
	if _, err := s.GetSandboxCA("sb1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing CA: %v", err)
	}
	when := time.Date(2026, 10, 4, 12, 0, 0, 0, time.FixedZone("west", -7*3600))
	ca := SandboxCA{SandboxID: "sb1", UserID: "alice", CertPEM: "cert1", KeyEnc: []byte{0, 1, 2}, CreatedAt: when}
	if err := s.PutSandboxCA(ca); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSandboxCA("sb1")
	if err != nil || got.SandboxID != ca.SandboxID || got.UserID != ca.UserID || got.CertPEM != ca.CertPEM || !reflect.DeepEqual(got.KeyEnc, ca.KeyEnc) || !got.CreatedAt.Equal(when) || got.CreatedAt.Location() != time.UTC {
		t.Fatalf("CA mismatch: %+v, %v", got, err)
	}
	ca.UserID, ca.CertPEM, ca.KeyEnc, ca.CreatedAt = "bob", "cert2", []byte{3, 4}, when.Add(time.Hour)
	if err := s.PutSandboxCA(ca); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetSandboxCA("sb1")
	if err != nil || got.UserID != ca.UserID || got.CertPEM != ca.CertPEM || !reflect.DeepEqual(got.KeyEnc, ca.KeyEnc) || !got.CreatedAt.Equal(ca.CreatedAt) {
		t.Fatalf("CA replacement mismatch: %+v, %v", got, err)
	}
	if err := s.DeleteSandboxCA("sb1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSandboxCA("sb1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("CA survived deletion: %v", err)
	}
}

func TestSandboxNetPolicyRoundTrip(t *testing.T) {
	s := testStore(t)
	policy := json.RawMessage(`{"default":"deny","allow":["api.github.com"]}`)
	for _, sb := range []Sandbox{
		{ID: "sb-policy", Name: "with-policy", CreatedBy: "alice", CreatedAt: time.Now(), NetPolicy: policy},
		{ID: "sb-legacy", Name: "without-policy", CreatedBy: "alice", CreatedAt: time.Now()},
	} {
		if err := s.CreateSandbox(sb); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetSandboxByID("sb-policy")
	if err != nil || !reflect.DeepEqual(got.NetPolicy, policy) {
		t.Fatalf("policy did not round-trip: %+v, %v", got, err)
	}
	got, err = s.GetSandboxByID("sb-legacy")
	if err != nil || got.NetPolicy != nil {
		t.Fatalf("legacy policy should be nil: %+v, %v", got, err)
	}
}

func TestGrantSchemaReopenIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	putGrant(t, s, grantFixture("g1", "alice", "GH", "sb1", "p1", time.Now()))
	if err := s.PutSandboxCA(SandboxCA{SandboxID: "sb1", UserID: "alice", CertPEM: "cert", KeyEnc: []byte("encrypted")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.GetSecretGrant("alice", "g1"); err != nil {
		t.Fatalf("grant lost after reopen: %v", err)
	}
	if ca, err := s.GetSandboxCA("sb1"); err != nil || ca.CertPEM != "cert" {
		t.Fatalf("CA lost after reopen: %+v, %v", ca, err)
	}
}
