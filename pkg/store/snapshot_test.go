package store

import (
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func snapshotFixture(id, user, name string) SnapshotRecord {
	return SnapshotRecord{
		ID: id, UserID: user, Name: name, SourceSandbox: "sb1",
		MemPath: "/x/" + name + "/mem.snap", ManifestJSON: `{"type":"memory"}`, SizeMB: 5,
		CreatedAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC),
	}
}

func rowCount(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSnapshotKeepsItsSourceAndSubstitution: what the source sandbox's record
// and its credential substitution looked like when the snapshot was taken
// comes back from GetSnapshot (and only from it), times in UTC.
func TestSnapshotKeepsItsSourceAndSubstitution(t *testing.T) {
	s := testStore(t)
	west := time.FixedZone("west", -7*3600)
	expires := time.Date(2026, 10, 9, 8, 0, 0, 0, west)
	snap := snapshotFixture("s1", "alice", "base")
	snap.Source = SnapshotSource{Image: "browser", DiskSizeMB: 4096, HasInit: true}
	snap.Substitution = SnapshotSubstitution{
		CA: &SandboxCA{UserID: "ignored", CertPEM: "cert", KeyEnc: []byte{1, 2, 3}, CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, west)},
		Grants: []SnapshotGrant{
			{SecretName: "GH_TOKEN", Hosts: []string{"api.github.com", "*.githubusercontent.com"}, Placeholder: "ph-b", ExpiresAt: &expires},
			{SecretName: "API", Hosts: []string{"a.example"}, Placeholder: "ph-a"},
		},
	}
	if err := s.CreateSnapshot(snap); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetSnapshot("alice", "base")
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != snap.Source || got.SourceSandbox != "sb1" || got.SizeMB != 5 {
		t.Fatalf("snapshot %+v", got)
	}
	if at := got.SourceRecordedAt; at == nil || at.Location() != time.UTC || time.Since(*at) > time.Minute || time.Until(*at) > time.Minute {
		t.Fatalf("recorded at %v, want now in UTC", at)
	}
	ca := got.Substitution.CA
	if ca == nil || ca.UserID != "alice" || ca.CertPEM != "cert" || !reflect.DeepEqual(ca.KeyEnc, []byte{1, 2, 3}) ||
		!ca.CreatedAt.Equal(snap.Substitution.CA.CreatedAt) || ca.CreatedAt.Location() != time.UTC {
		t.Fatalf("CA %+v", ca)
	}
	grants := got.Substitution.Grants
	if len(grants) != 2 || grants[0].SecretName != "API" || grants[0].ExpiresAt != nil ||
		grants[1].SecretName != "GH_TOKEN" || grants[1].Placeholder != "ph-b" ||
		!reflect.DeepEqual(grants[1].Hosts, []string{"api.github.com", "*.githubusercontent.com"}) ||
		grants[1].ExpiresAt == nil || !grants[1].ExpiresAt.Equal(expires) || grants[1].ExpiresAt.Location() != time.UTC {
		t.Fatalf("grants %+v", grants)
	}

	list, err := s.ListSnapshots("alice")
	if err != nil || len(list) != 1 || list[0].Source != snap.Source || list[0].Substitution.CA != nil || len(list[0].Substitution.Grants) != 0 ||
		list[0].SourceRecordedAt == nil {
		t.Fatalf("list %+v, %v: want the source spec, its marker and no substitution", list, err)
	}

	plain := snapshotFixture("s2", "alice", "plain")
	if err := s.CreateSnapshot(plain); err != nil {
		t.Fatal(err)
	}
	// One that kept nothing is still one that recorded: the marker, not an
	// empty record, tells it from a snapshot that predates recording.
	if got, err := s.GetSnapshot("alice", "plain"); err != nil || got.Source != (SnapshotSource{}) || got.Substitution.CA != nil ||
		len(got.Substitution.Grants) != 0 || got.SourceRecordedAt == nil {
		t.Fatalf("a snapshot that kept nothing: %+v, %v", got, err)
	}
}

// TestCreateSnapshotIsAtomic: the record and what it keeps go in together or
// not at all.
func TestCreateSnapshotIsAtomic(t *testing.T) {
	s := testStore(t)
	snap := snapshotFixture("s1", "alice", "base")
	snap.Substitution = SnapshotSubstitution{
		CA: &SandboxCA{CertPEM: "cert", KeyEnc: []byte("key")},
		Grants: []SnapshotGrant{
			{SecretName: "A", Hosts: []string{"a.example"}, Placeholder: "same"},
			{SecretName: "B", Hosts: []string{"b.example"}, Placeholder: "same"}, // a snapshot can't hold a placeholder twice
		},
	}
	if err := s.CreateSnapshot(snap); err == nil {
		t.Fatal("a snapshot with the same placeholder twice was accepted")
	}
	if _, err := s.GetSnapshot("alice", "base"); err == nil {
		t.Fatal("the snapshot record outlived its failed create")
	}
	for _, table := range []string{"snapshots", "snapshot_cas", "snapshot_grants"} {
		if n := rowCount(t, s, `SELECT COUNT(*) FROM `+table); n != 0 {
			t.Fatalf("%s has %d rows after a failed create", table, n)
		}
	}

	// A name clash fails on the record, before anything is kept for it.
	if err := s.CreateSnapshot(snapshotFixture("s2", "alice", "base")); err != nil {
		t.Fatal(err)
	}
	dup := snapshotFixture("s3", "alice", "base")
	dup.Substitution = SnapshotSubstitution{CA: &SandboxCA{CertPEM: "cert", KeyEnc: []byte("key")}}
	if err := s.CreateSnapshot(dup); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("duplicate name: %v, want a UNIQUE violation callers can recognize", err)
	}
	if n := rowCount(t, s, `SELECT COUNT(*) FROM snapshot_cas`); n != 0 {
		t.Fatalf("a refused snapshot kept its CA")
	}
}

// TestDeleteSnapshotDropsWhatItKept: deleting a snapshot takes its CA and
// grants with it, and only its own.
func TestDeleteSnapshotDropsWhatItKept(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{"s1", "s2"} {
		snap := snapshotFixture(id, "alice", "snap-"+id)
		snap.Substitution = SnapshotSubstitution{
			CA:     &SandboxCA{CertPEM: "cert-" + id, KeyEnc: []byte("key")},
			Grants: []SnapshotGrant{{SecretName: "GH_TOKEN", Hosts: []string{"api.github.com"}, Placeholder: "ph-" + id}},
		}
		if err := s.CreateSnapshot(snap); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteSnapshot("bob", "snap-s1"); err == nil {
		t.Fatal("another user deleted the snapshot")
	}
	if err := s.DeleteSnapshot("alice", "snap-s1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSnapshot("alice", "snap-s1"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("second delete: %v", err)
	}
	if n := rowCount(t, s, `SELECT COUNT(*) FROM snapshot_cas WHERE snapshot_id = 's1'`) +
		rowCount(t, s, `SELECT COUNT(*) FROM snapshot_grants WHERE snapshot_id = 's1'`); n != 0 {
		t.Fatalf("a deleted snapshot left %d CA/grant rows", n)
	}
	got, err := s.GetSnapshot("alice", "snap-s2")
	if err != nil || got.Substitution.CA == nil || len(got.Substitution.Grants) != 1 {
		t.Fatalf("the other snapshot's CA and grants: %+v, %v", got, err)
	}

	// A new snapshot under the deleted name starts with nothing of the old one.
	if err := s.CreateSnapshot(snapshotFixture("s3", "alice", "snap-s1")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetSnapshot("alice", "snap-s1"); err != nil || got.Substitution.CA != nil || len(got.Substitution.Grants) != 0 {
		t.Fatalf("a re-created snapshot inherited its predecessor's substitution: %+v, %v", got, err)
	}
}

// TestDeleteSecretGrantsForSecretReachesSnapshots: deleting a secret drops the
// grants snapshots keep for it, so a secret re-created under the name can't
// come back through a resume; other secrets and other users' are untouched.
func TestDeleteSecretGrantsForSecretReachesSnapshots(t *testing.T) {
	s := testStore(t)
	for _, c := range []struct{ id, user string }{{"s1", "alice"}, {"s2", "bob"}} {
		snap := snapshotFixture(c.id, c.user, "base")
		snap.Substitution = SnapshotSubstitution{Grants: []SnapshotGrant{
			{SecretName: "GH_TOKEN", Hosts: []string{"api.github.com"}, Placeholder: "gh-" + c.id},
			{SecretName: "OTHER", Hosts: []string{"o.example"}, Placeholder: "o-" + c.id},
		}}
		if err := s.CreateSnapshot(snap); err != nil {
			t.Fatal(err)
		}
	}
	putGrant(t, s, grantFixture("g1", "alice", "GH_TOKEN", "sb1", "gh-live", time.Now()))

	if err := s.DeleteSecretGrantsForSecret("alice", "GH_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSecretGrant("alice", "g1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the sandbox's grant survived: %v", err)
	}
	names := func(user string) []string {
		snap, err := s.GetSnapshot(user, "base")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, g := range snap.Substitution.Grants {
			out = append(out, g.SecretName)
		}
		return out
	}
	if got := names("alice"); !reflect.DeepEqual(got, []string{"OTHER"}) {
		t.Fatalf("alice's snapshot keeps %v, want only OTHER", got)
	}
	if got := names("bob"); !reflect.DeepEqual(got, []string{"GH_TOKEN", "OTHER"}) {
		t.Fatalf("bob's snapshot keeps %v, want both", got)
	}
}
