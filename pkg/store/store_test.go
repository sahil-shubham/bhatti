package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTemplatesCRUD(t *testing.T) {
	s := testStore(t)

	tmpl := Template{
		ID:        "t1",
		Name:      "ubuntu-dev",
		Engine:    "docker",
		Image:     "ubuntu:22.04",
		CPUs:      2,
		MemoryMB:  1024,
		Secrets:   []string{"github-token"},
		Labels:    map[string]string{"env": "dev"},
		CreatedAt: time.Now().Truncate(time.Second),
	}

	// Create
	if err := s.CreateTemplate(tmpl); err != nil {
		t.Fatal(err)
	}

	// Get
	got, err := s.GetTemplate("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "ubuntu-dev" || got.Image != "ubuntu:22.04" || got.CPUs != 2 || got.MemoryMB != 1024 {
		t.Fatalf("unexpected template: %+v", got)
	}
	if len(got.Secrets) != 1 || got.Secrets[0] != "github-token" {
		t.Fatalf("unexpected secrets: %v", got.Secrets)
	}
	if got.Labels["env"] != "dev" {
		t.Fatalf("unexpected labels: %v", got.Labels)
	}

	// List
	list, err := s.ListTemplates()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 template, got %d", len(list))
	}

	// Delete
	if err := s.DeleteTemplate("t1"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListTemplates()
	if len(list) != 0 {
		t.Fatal("expected 0 templates after delete")
	}

	// Delete non-existent
	if err := s.DeleteTemplate("nope"); err == nil {
		t.Fatal("expected error deleting non-existent template")
	}
}

func TestSandboxesCRUD(t *testing.T) {
	s := testStore(t)

	sb := Sandbox{
		ID:         "s1",
		Name:       "my-sandbox",
		TemplateID: "t1",
		EngineID:   "abc123",
		Status:     "running",
		IP:         "172.17.0.2",
		EngineMeta: json.RawMessage(`{"port":8080}`),
		CreatedBy:  "usr_alice",
		CreatedAt:  time.Now().Truncate(time.Second),
	}

	// Create
	if err := s.CreateSandbox(sb); err != nil {
		t.Fatal(err)
	}

	// Get (scoped)
	got, err := s.GetSandbox("usr_alice", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "my-sandbox" || got.Status != "running" || got.IP != "172.17.0.2" {
		t.Fatalf("unexpected sandbox: %+v", got)
	}
	if got.CreatedBy != "usr_alice" {
		t.Fatalf("expected created_by usr_alice, got %s", got.CreatedBy)
	}

	// Get (wrong user)
	_, err = s.GetSandbox("usr_bob", "s1")
	if err == nil {
		t.Fatal("expected error getting sandbox with wrong user")
	}

	// Get (unscoped)
	got, err = s.GetSandboxByID("s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "my-sandbox" {
		t.Fatalf("unexpected sandbox via GetSandboxByID: %+v", got)
	}

	// Update status
	if err := s.UpdateSandboxStatus("s1", "stopped"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSandbox("usr_alice", "s1")
	if got.Status != "stopped" {
		t.Fatalf("expected stopped, got %s", got.Status)
	}

	// Stop (sets stopped_at)
	if err := s.StopSandbox("s1"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSandbox("usr_alice", "s1")
	if got.StoppedAt == nil {
		t.Fatal("expected stopped_at to be set")
	}

	// List (scoped)
	list, err := s.ListSandboxes("usr_alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 sandbox, got %d", len(list))
	}

	// List (wrong user)
	list, _ = s.ListSandboxes("usr_bob")
	if len(list) != 0 {
		t.Fatalf("expected 0 sandboxes for bob, got %d", len(list))
	}

	// ListAll
	all, _ := s.ListAllSandboxes()
	if len(all) != 1 {
		t.Fatalf("expected 1 sandbox in ListAll, got %d", len(all))
	}

	// Delete (wrong user)
	if err := s.DeleteSandbox("usr_bob", "s1"); err == nil {
		t.Fatal("expected error deleting sandbox with wrong user")
	}

	// Delete (correct user)
	if err := s.DeleteSandbox("usr_alice", "s1"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListSandboxes("usr_alice")
	if len(list) != 0 {
		t.Fatal("expected 0 sandboxes after delete")
	}
}

func TestSecretsCRUD(t *testing.T) {
	s := testStore(t)

	encrypted := []byte("fake-encrypted-data")

	if err := s.SetSecret("usr_alice", "api-key", encrypted); err != nil {
		t.Fatal(err)
	}

	// Get value (correct user)
	got, err := s.GetSecretValue("usr_alice", "api-key")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(encrypted) {
		t.Fatalf("expected %q, got %q", encrypted, got)
	}

	// Get value (wrong user)
	_, err = s.GetSecretValue("usr_bob", "api-key")
	if err == nil {
		t.Fatal("expected error getting secret with wrong user")
	}

	// Get metadata (no value)
	meta, err := s.GetSecret("usr_alice", "api-key")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "api-key" {
		t.Fatalf("expected name 'api-key', got %q", meta.Name)
	}

	// Update
	updated := []byte("new-encrypted-data")
	if err := s.SetSecret("usr_alice", "api-key", updated); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSecretValue("usr_alice", "api-key")
	if string(got) != string(updated) {
		t.Fatalf("after update: expected %q, got %q", updated, got)
	}

	// List (scoped)
	list, err := s.ListUserSecrets("usr_alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 secret, got %d", len(list))
	}

	// List (wrong user)
	list, _ = s.ListUserSecrets("usr_bob")
	if len(list) != 0 {
		t.Fatalf("expected 0 secrets for bob, got %d", len(list))
	}

	// Delete (wrong user)
	if err := s.DeleteSecret("usr_bob", "api-key"); err == nil {
		t.Fatal("expected error deleting secret with wrong user")
	}

	// Delete (correct user)
	if err := s.DeleteSecret("usr_alice", "api-key"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListUserSecrets("usr_alice")
	if len(list) != 0 {
		t.Fatal("expected 0 secrets after delete")
	}
}

func TestSandboxNilEngineMeta(t *testing.T) {
	s := testStore(t)

	sb := Sandbox{
		ID:        "s2",
		Name:      "no-meta",
		Status:    "running",
		CreatedBy: "usr_test",
		CreatedAt: time.Now(),
	}
	if err := s.CreateSandbox(sb); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSandbox("usr_test", "s2")
	if string(got.EngineMeta) != "{}" {
		t.Fatalf("expected empty JSON object, got %s", got.EngineMeta)
	}
}

func TestVolumesCRUD(t *testing.T) {
	s := testStore(t)

	// Create
	if err := s.CreateVolume("my-vol"); err != nil {
		t.Fatal(err)
	}

	// Create is idempotent
	if err := s.CreateVolume("my-vol"); err != nil {
		t.Fatal(err)
	}

	// Get
	vol, err := s.GetVolume("my-vol")
	if err != nil {
		t.Fatal(err)
	}
	if vol.Name != "my-vol" {
		t.Fatalf("expected my-vol, got %s", vol.Name)
	}

	// List
	list, err := s.ListVolumes()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(list))
	}

	// Delete
	if err := s.DeleteVolume("my-vol"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListVolumes()
	if len(list) != 0 {
		t.Fatal("expected 0 volumes after delete")
	}

	// Delete non-existent
	if err := s.DeleteVolume("nope"); err == nil {
		t.Fatal("expected error deleting non-existent volume")
	}
}

func TestVolumeDeleteBlockedByAttachment(t *testing.T) {
	s := testStore(t)

	s.CreateVolume("shared-vol")

	// Create a sandbox so we can attach to it
	sb := Sandbox{
		ID:        "s-vol-test",
		Name:      "vol-sandbox",
		Status:    "running",
		CreatedBy: "usr_test",
		CreatedAt: time.Now(),
	}
	s.CreateSandbox(sb)

	// Attach
	if err := s.AttachVolume("s-vol-test", "shared-vol", "/data", false); err != nil {
		t.Fatal(err)
	}

	// Delete should fail
	if err := s.DeleteVolume("shared-vol"); err == nil {
		t.Fatal("expected error deleting volume in use")
	}

	// Detach then delete
	s.DetachVolumes("s-vol-test")
	if err := s.DeleteVolume("shared-vol"); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxVolumes(t *testing.T) {
	s := testStore(t)

	sb := Sandbox{
		ID:        "s-sv-test",
		Name:      "sv-sandbox",
		Status:    "running",
		CreatedBy: "usr_test",
		CreatedAt: time.Now(),
	}
	s.CreateSandbox(sb)

	// Attach volumes
	s.AttachVolume("s-sv-test", "vol-a", "/mnt/a", false)
	s.AttachVolume("s-sv-test", "vol-b", "/mnt/b", true)

	vols, err := s.GetSandboxVolumes("s-sv-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(vols))
	}

	// Check readonly flag
	for _, v := range vols {
		if v.VolumeName == "vol-b" && !v.ReadOnly {
			t.Fatal("expected vol-b to be readonly")
		}
		if v.VolumeName == "vol-a" && v.ReadOnly {
			t.Fatal("expected vol-a to be read-write")
		}
	}

	// Detach
	s.DetachVolumes("s-sv-test")
	vols, _ = s.GetSandboxVolumes("s-sv-test")
	if len(vols) != 0 {
		t.Fatal("expected 0 volumes after detach")
	}
}

func TestTemplateMounts(t *testing.T) {
	s := testStore(t)

	tmpl := Template{
		ID:       "t-mounts",
		Name:     "with-mounts",
		Engine:   "docker",
		Image:    "ubuntu:22.04",
		CPUs:     1,
		MemoryMB: 512,
		Mounts: []TemplateMountSpec{
			{VolumeName: "shared-data", Target: "/data", ReadOnly: false, AutoCreate: true},
			{Target: "/workspace", AutoCreate: true}, // VolumeName empty = auto-generate
		},
		CreatedAt: time.Now().Truncate(time.Second),
	}

	if err := s.CreateTemplate(tmpl); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetTemplate("t-mounts")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(got.Mounts))
	}
	if got.Mounts[0].VolumeName != "shared-data" {
		t.Fatalf("expected shared-data, got %s", got.Mounts[0].VolumeName)
	}
	if got.Mounts[0].Target != "/data" {
		t.Fatalf("expected /data, got %s", got.Mounts[0].Target)
	}
	if got.Mounts[1].Target != "/workspace" {
		t.Fatalf("expected /workspace, got %s", got.Mounts[1].Target)
	}
	if !got.Mounts[1].AutoCreate {
		t.Fatal("expected auto_create to be true")
	}
}

func TestEnsureKeypair(t *testing.T) {
	// Import from parent package — tested separately
	_ = os.TempDir()
}

// --- User Tests (Part 1 of PLAN-v5) ---

func TestUserCRUD(t *testing.T) {
	s := testStore(t)

	u := User{
		ID:                    "usr_1",
		Name:                  "alice",
		APIKeyHash:            "abc123hash",
		MaxSandboxes:          5,
		MaxCPUsPerSandbox:     4,
		MaxMemoryMBPerSandbox: 4096,
		SubnetIndex:           1,
		CreatedAt:             time.Now().Truncate(time.Second),
	}

	// Create
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}

	// Get by ID
	got, err := s.GetUser("usr_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "alice" || got.MaxSandboxes != 5 || got.SubnetIndex != 1 {
		t.Fatalf("unexpected user: %+v", got)
	}

	// Get by key hash
	got, err = s.GetUserByKeyHash("abc123hash")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "usr_1" {
		t.Fatalf("expected usr_1, got %s", got.ID)
	}

	// Get by wrong hash
	_, err = s.GetUserByKeyHash("wronghash")
	if err == nil {
		t.Fatal("expected error for wrong hash")
	}

	// List
	list, err := s.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 user, got %d", len(list))
	}

	// Delete (no active sandboxes)
	if err := s.DeleteUser("usr_1"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListUsers()
	if len(list) != 0 {
		t.Fatal("expected 0 users after delete")
	}

	// Delete non-existent
	if err := s.DeleteUser("usr_nope"); err == nil {
		t.Fatal("expected error deleting non-existent user")
	}
}

func TestUserDeleteRefusedWithSandboxes(t *testing.T) {
	s := testStore(t)

	s.CreateUser(User{
		ID: "usr_del", Name: "del-test", APIKeyHash: "hash1",
		MaxSandboxes: 5, SubnetIndex: 1, CreatedAt: time.Now(),
	})

	s.CreateSandbox(Sandbox{
		ID: "sb_del", Name: "del-sandbox", Status: "running",
		CreatedBy: "usr_del", CreatedAt: time.Now(),
	})

	err := s.DeleteUser("usr_del")
	if err == nil {
		t.Fatal("expected error deleting user with active sandboxes")
	}
	if !strings.Contains(err.Error(), "active sandbox") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUserDeleteRefusedWithSecrets(t *testing.T) {
	s := testStore(t)

	s.CreateUser(User{
		ID: "usr_sec", Name: "sec-test", APIKeyHash: "hash2",
		MaxSandboxes: 5, SubnetIndex: 1, CreatedAt: time.Now(),
	})

	s.SetSecret("usr_sec", "my-key", []byte("encrypted"))

	err := s.DeleteUser("usr_sec")
	if err == nil {
		t.Fatal("expected error deleting user with secrets")
	}
	if !strings.Contains(err.Error(), "secret") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestKeyRotation(t *testing.T) {
	s := testStore(t)

	s.CreateUser(User{
		ID: "usr_rot", Name: "rot-test", APIKeyHash: "oldhash",
		MaxSandboxes: 5, SubnetIndex: 1, CreatedAt: time.Now(),
	})

	// Rotate key
	if err := s.RotateUserKey("usr_rot", "newhash"); err != nil {
		t.Fatal(err)
	}

	// Old hash should not work
	_, err := s.GetUserByKeyHash("oldhash")
	if err == nil {
		t.Fatal("expected old hash to not work after rotation")
	}

	// New hash should work
	u, err := s.GetUserByKeyHash("newhash")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "usr_rot" {
		t.Fatalf("expected usr_rot, got %s", u.ID)
	}

	// Rotate non-existent user
	if err := s.RotateUserKey("usr_nope", "hash"); err == nil {
		t.Fatal("expected error rotating non-existent user")
	}
}

func TestNextSubnetIndex(t *testing.T) {
	s := testStore(t)

	// No users → first subnet is 1
	idx, err := s.NextSubnetIndex()
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("expected 1, got %d", idx)
	}

	// Create user with subnet 1
	s.CreateUser(User{
		ID: "usr_sub1", Name: "sub1", APIKeyHash: "h1",
		SubnetIndex: 1, CreatedAt: time.Now(),
	})

	idx, err = s.NextSubnetIndex()
	if err != nil {
		t.Fatal(err)
	}
	if idx != 2 {
		t.Fatalf("expected 2, got %d", idx)
	}

	// Create user with subnet 5 (gap)
	s.CreateUser(User{
		ID: "usr_sub5", Name: "sub5", APIKeyHash: "h5",
		SubnetIndex: 5, CreatedAt: time.Now(),
	})

	idx, err = s.NextSubnetIndex()
	if err != nil {
		t.Fatal(err)
	}
	if idx != 6 {
		t.Fatalf("expected 6, got %d", idx)
	}
}

func TestSandboxNameUniquenessPerUser(t *testing.T) {
	s := testStore(t)

	// Create two sandboxes with same name for same user
	s.CreateSandbox(Sandbox{
		ID: "sb1", Name: "dev", Status: "running",
		CreatedBy: "usr_alice", CreatedAt: time.Now(),
	})

	err := s.CreateSandbox(Sandbox{
		ID: "sb2", Name: "dev", Status: "running",
		CreatedBy: "usr_alice", CreatedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("expected error creating duplicate sandbox name for same user")
	}

	// Different user can use the same name
	err = s.CreateSandbox(Sandbox{
		ID: "sb3", Name: "dev", Status: "running",
		CreatedBy: "usr_bob", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("expected different user to create same name: %v", err)
	}

	// After destroying, same user can reuse the name
	s.UpdateSandboxStatus("sb1", "destroyed")
	err = s.CreateSandbox(Sandbox{
		ID: "sb4", Name: "dev", Status: "running",
		CreatedBy: "usr_alice", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("expected name reuse after destroy: %v", err)
	}
}

func TestRenameSandbox(t *testing.T) {
	s := testStore(t)

	// Happy path: rename a single sandbox, lookup by old name fails, by new
	// name succeeds.
	if err := s.CreateSandbox(Sandbox{
		ID: "r1", Name: "foo", Status: "running",
		CreatedBy: "usr_alice", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameSandbox("usr_alice", "r1", "bar"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, err := s.GetSandbox("usr_alice", "bar")
	if err != nil {
		t.Fatalf("get by new name: %v", err)
	}
	if got.ID != "r1" || got.Name != "bar" {
		t.Fatalf("after rename: id=%s name=%s", got.ID, got.Name)
	}
	if _, err := s.GetSandbox("usr_alice", "foo"); err == nil {
		t.Fatal("expected lookup by old name to fail")
	}
}

func TestRenameSandbox_Conflict(t *testing.T) {
	s := testStore(t)

	s.CreateSandbox(Sandbox{
		ID: "a", Name: "foo", Status: "running",
		CreatedBy: "usr_alice", CreatedAt: time.Now(),
	})
	s.CreateSandbox(Sandbox{
		ID: "b", Name: "bar", Status: "running",
		CreatedBy: "usr_alice", CreatedAt: time.Now(),
	})

	err := s.RenameSandbox("usr_alice", "a", "bar")
	if err == nil {
		t.Fatal("expected UNIQUE conflict")
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("expected UNIQUE in error, got: %v", err)
	}

	// 'a' still exists with its original name
	got, err := s.GetSandbox("usr_alice", "a")
	if err != nil || got.Name != "foo" {
		t.Fatalf("after failed rename, expected name=foo, got name=%s err=%v", got.Name, err)
	}
}

func TestRenameSandbox_DestroyedNameReusable(t *testing.T) {
	s := testStore(t)

	// 'foo' exists but is destroyed; the partial unique index should let us
	// rename another sandbox to 'foo'.
	s.CreateSandbox(Sandbox{
		ID: "old", Name: "foo", Status: "running",
		CreatedBy: "usr_alice", CreatedAt: time.Now(),
	})
	s.UpdateSandboxStatus("old", "destroyed")

	s.CreateSandbox(Sandbox{
		ID: "new", Name: "bar", Status: "running",
		CreatedBy: "usr_alice", CreatedAt: time.Now(),
	})
	if err := s.RenameSandbox("usr_alice", "new", "foo"); err != nil {
		t.Fatalf("rename to destroyed-sandbox name: %v", err)
	}
}

func TestRenameSandbox_OtherUserCannotRename(t *testing.T) {
	s := testStore(t)

	s.CreateSandbox(Sandbox{
		ID: "x", Name: "foo", Status: "running",
		CreatedBy: "usr_alice", CreatedAt: time.Now(),
	})

	// Bob cannot rename Alice's sandbox — rows-affected is zero.
	err := s.RenameSandbox("usr_bob", "x", "bar")
	if err == nil {
		t.Fatal("expected not-found error for cross-user rename")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found', got: %v", err)
	}

	// Original is unchanged.
	got, _ := s.GetSandbox("usr_alice", "x")
	if got.Name != "foo" {
		t.Fatalf("sandbox name was modified by other user: %s", got.Name)
	}
}

func TestCountUserSandboxes(t *testing.T) {
	s := testStore(t)

	s.CreateSandbox(Sandbox{
		ID: "c1", Name: "one", Status: "running",
		CreatedBy: "usr_cnt", CreatedAt: time.Now(),
	})
	s.CreateSandbox(Sandbox{
		ID: "c2", Name: "two", Status: "stopped",
		CreatedBy: "usr_cnt", CreatedAt: time.Now(),
	})
	s.CreateSandbox(Sandbox{
		ID: "c3", Name: "three", Status: "destroyed",
		CreatedBy: "usr_cnt", CreatedAt: time.Now(),
	})

	count, err := s.CountUserSandboxes("usr_cnt")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 non-destroyed sandboxes, got %d", count)
	}

	// Different user
	count, _ = s.CountUserSandboxes("usr_other")
	if count != 0 {
		t.Fatalf("expected 0 for other user, got %d", count)
	}
}

func TestSandboxScoping(t *testing.T) {
	s := testStore(t)

	// Alice creates a sandbox
	s.CreateSandbox(Sandbox{
		ID: "scope1", Name: "alice-dev", Status: "running",
		CreatedBy: "usr_alice", CreatedAt: time.Now(),
	})

	// Bob creates a sandbox
	s.CreateSandbox(Sandbox{
		ID: "scope2", Name: "bob-dev", Status: "running",
		CreatedBy: "usr_bob", CreatedAt: time.Now(),
	})

	// Alice lists: sees only hers
	aliceList, _ := s.ListSandboxes("usr_alice")
	if len(aliceList) != 1 || aliceList[0].ID != "scope1" {
		t.Fatalf("alice should see 1 sandbox, got %d", len(aliceList))
	}

	// Bob lists: sees only his
	bobList, _ := s.ListSandboxes("usr_bob")
	if len(bobList) != 1 || bobList[0].ID != "scope2" {
		t.Fatalf("bob should see 1 sandbox, got %d", len(bobList))
	}

	// Alice can't get Bob's sandbox
	_, err := s.GetSandbox("usr_alice", "scope2")
	if err == nil {
		t.Fatal("alice should not be able to get bob's sandbox")
	}

	// Alice can't delete Bob's sandbox
	err = s.DeleteSandbox("usr_alice", "scope2")
	if err == nil {
		t.Fatal("alice should not be able to delete bob's sandbox")
	}

	// ListAll sees both
	all, _ := s.ListAllSandboxes()
	if len(all) != 2 {
		t.Fatalf("expected 2 sandboxes in ListAll, got %d", len(all))
	}
}

func TestSecretScoping(t *testing.T) {
	s := testStore(t)

	s.SetSecret("usr_alice", "alice-key", []byte("alice-data"))
	s.SetSecret("usr_bob", "bob-key", []byte("bob-data"))

	// Alice sees only hers
	aliceList, _ := s.ListUserSecrets("usr_alice")
	if len(aliceList) != 1 || aliceList[0].Name != "alice-key" {
		t.Fatalf("alice should see 1 secret, got %v", aliceList)
	}

	// Alice can't read Bob's
	_, err := s.GetSecretValue("usr_alice", "bob-key")
	if err == nil {
		t.Fatal("alice should not be able to read bob's secret")
	}

	// Alice can't delete Bob's
	err = s.DeleteSecret("usr_alice", "bob-key")
	if err == nil {
		t.Fatal("alice should not be able to delete bob's secret")
	}

	// ListAll sees both
	allSecrets, _ := s.ListAllSecrets()
	if len(allSecrets) != 2 {
		t.Fatalf("expected 2 secrets in ListAll, got %d", len(allSecrets))
	}
}

// --- Bug exposure tests ---

func TestTwoUsersCreateSameSecretName(t *testing.T) {
	s := testStore(t)

	// Alice creates "API_KEY"
	if err := s.SetSecret("usr_alice", "API_KEY", []byte("alice-data")); err != nil {
		t.Fatal(err)
	}

	// Bob creates "API_KEY" — should succeed (different user namespace)
	if err := s.SetSecret("usr_bob", "API_KEY", []byte("bob-data")); err != nil {
		t.Fatalf("bob should be able to create same secret name: %v", err)
	}

	// Each user should see their own value
	aliceVal, err := s.GetSecretValue("usr_alice", "API_KEY")
	if err != nil {
		t.Fatalf("alice get: %v", err)
	}
	if string(aliceVal) != "alice-data" {
		t.Fatalf("alice value: %q, want 'alice-data'", aliceVal)
	}

	bobVal, err := s.GetSecretValue("usr_bob", "API_KEY")
	if err != nil {
		t.Fatalf("bob get: %v", err)
	}
	if string(bobVal) != "bob-data" {
		t.Fatalf("bob value: %q, want 'bob-data'", bobVal)
	}

	// Each user should see exactly 1 secret
	aliceList, _ := s.ListUserSecrets("usr_alice")
	if len(aliceList) != 1 {
		t.Fatalf("alice should have 1 secret, got %d", len(aliceList))
	}
	bobList, _ := s.ListUserSecrets("usr_bob")
	if len(bobList) != 1 {
		t.Fatalf("bob should have 1 secret, got %d", len(bobList))
	}
}

func TestDeleteSandboxByID(t *testing.T) {
	s := testStore(t)

	s.CreateSandbox(Sandbox{
		ID: "del-byid", Name: "byid-test", Status: "running",
		CreatedBy: "usr_alice", CreatedAt: time.Now(),
	})

	// DeleteSandboxByID doesn't require user scoping
	if err := s.DeleteSandboxByID("del-byid"); err != nil {
		t.Fatal(err)
	}

	// Verify gone
	_, err := s.GetSandboxByID("del-byid")
	if err == nil {
		t.Fatal("expected error after delete")
	}

	// Delete non-existent
	if err := s.DeleteSandboxByID("nope"); err == nil {
		t.Fatal("expected error deleting non-existent")
	}
}

func TestUserDuplicateNameRejected(t *testing.T) {
	s := testStore(t)

	s.CreateUser(User{
		ID: "usr_dup1", Name: "alice", APIKeyHash: "hash_a",
		MaxSandboxes: 5, SubnetIndex: 1, CreatedAt: time.Now(),
	})

	err := s.CreateUser(User{
		ID: "usr_dup2", Name: "alice", APIKeyHash: "hash_b",
		MaxSandboxes: 5, SubnetIndex: 2, CreatedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("expected error creating user with duplicate name")
	}
}

func TestUserDuplicateKeyHashRejected(t *testing.T) {
	s := testStore(t)

	s.CreateUser(User{
		ID: "usr_kdup1", Name: "alice", APIKeyHash: "same_hash",
		MaxSandboxes: 5, SubnetIndex: 1, CreatedAt: time.Now(),
	})

	err := s.CreateUser(User{
		ID: "usr_kdup2", Name: "bob", APIKeyHash: "same_hash",
		MaxSandboxes: 5, SubnetIndex: 2, CreatedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("expected error creating user with duplicate key hash")
	}
}

// ==========================================================================
// v0.3 Persistent Volume Tests
// ==========================================================================

func createTestUser(t *testing.T, s *Store, id, name string) {
	t.Helper()
	s.CreateUser(User{
		ID: id, Name: name, APIKeyHash: "hash_" + id,
		MaxSandboxes: 10, MaxCPUsPerSandbox: 4, MaxMemoryMBPerSandbox: 4096,
		SubnetIndex: 1, CreatedAt: time.Now(),
	})
}

func createTestVolume(t *testing.T, s *Store, userID, name string, sizeMB int) PersistentVolume {
	t.Helper()
	v := PersistentVolume{
		ID: "vol_" + userID + "_" + name, UserID: userID, Name: name,
		SizeMB: sizeMB, FilePath: "/tmp/" + userID + "/" + name + ".ext4",
		Status: "ready", CreatedAt: time.Now(),
	}
	if err := s.CreatePersistentVolume(v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestShellTokenRoundTrip(t *testing.T) {
	s := testStore(t)

	s.CreateSandbox(Sandbox{
		ID: "sb_shell", Name: "shell-test", Status: "running",
		CreatedBy: "usr_alice", CreatedAt: time.Now(),
	})

	// Initially empty
	sb, err := s.GetSandboxByID("sb_shell")
	if err != nil {
		t.Fatal(err)
	}
	if sb.ShellTokenHash != "" {
		t.Fatalf("expected empty shell_token_hash, got %q", sb.ShellTokenHash)
	}

	// Set token
	if err := s.SetShellToken("sb_shell", "abc123hash"); err != nil {
		t.Fatal(err)
	}
	sb, _ = s.GetSandboxByID("sb_shell")
	if sb.ShellTokenHash != "abc123hash" {
		t.Fatalf("expected abc123hash, got %q", sb.ShellTokenHash)
	}

	// Rotate (set again)
	if err := s.SetShellToken("sb_shell", "newhash456"); err != nil {
		t.Fatal(err)
	}
	sb, _ = s.GetSandboxByID("sb_shell")
	if sb.ShellTokenHash != "newhash456" {
		t.Fatalf("expected newhash456, got %q", sb.ShellTokenHash)
	}

	// Clear (revoke)
	if err := s.ClearShellToken("sb_shell"); err != nil {
		t.Fatal(err)
	}
	sb, _ = s.GetSandboxByID("sb_shell")
	if sb.ShellTokenHash != "" {
		t.Fatalf("expected empty after clear, got %q", sb.ShellTokenHash)
	}

	// Also accessible via user-scoped GetSandbox
	s.SetShellToken("sb_shell", "scopedhash")
	sb, _ = s.GetSandbox("usr_alice", "sb_shell")
	if sb.ShellTokenHash != "scopedhash" {
		t.Fatalf("expected scopedhash via GetSandbox, got %q", sb.ShellTokenHash)
	}
}
