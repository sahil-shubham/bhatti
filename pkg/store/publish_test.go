package store

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCreatePublishRule(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestSandbox(t, s, "usr_a", "sb1", "my-app")

	rule := PublishRule{
		ID: "pub_1", SandboxID: "sb1", UserID: "usr_a",
		Port: 3000, Alias: "my-app",
	}
	if err := s.CreatePublishRule(rule); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetPublishRuleByAlias("my-app")
	if err != nil {
		t.Fatal(err)
	}
	if got.SandboxID != "sb1" || got.Port != 3000 || got.Alias != "my-app" {
		t.Errorf("got %+v", got)
	}
}

func TestPublishRuleAliasUnique(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestSandbox(t, s, "usr_a", "sb1", "app1")
	createTestSandbox(t, s, "usr_a", "sb2", "app2")

	s.CreatePublishRule(PublishRule{ID: "pub_1", SandboxID: "sb1", UserID: "usr_a", Port: 3000, Alias: "my-alias"})

	err := s.CreatePublishRule(PublishRule{ID: "pub_2", SandboxID: "sb2", UserID: "usr_a", Port: 3000, Alias: "my-alias"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("alias already in use: %v, want ErrConflict", err)
	}
	if got, err := s.GetPublishRuleByAlias("my-alias"); err != nil || got.SandboxID != "sb1" {
		t.Fatalf("the refused rule took the alias over: %+v, %v", got, err)
	}
}

func TestPublishRuleSandboxPortUnique(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestSandbox(t, s, "usr_a", "sb1", "app1")

	s.CreatePublishRule(PublishRule{ID: "pub_1", SandboxID: "sb1", UserID: "usr_a", Port: 3000, Alias: "alias-a"})

	err := s.CreatePublishRule(PublishRule{ID: "pub_2", SandboxID: "sb1", UserID: "usr_a", Port: 3000, Alias: "alias-b"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("port already published: %v, want ErrConflict", err)
	}
	if rules, err := s.ListPublishRules("sb1"); err != nil || len(rules) != 1 || rules[0].Alias != "alias-a" {
		t.Fatalf("the refused rule changed what the sandbox publishes: %+v, %v", rules, err)
	}
}

func TestPublishRuleDifferentSandboxSamePort(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestSandbox(t, s, "usr_a", "sb1", "app1")
	createTestSandbox(t, s, "usr_a", "sb2", "app2")

	s.CreatePublishRule(PublishRule{ID: "pub_1", SandboxID: "sb1", UserID: "usr_a", Port: 3000, Alias: "app1"})
	if err := s.CreatePublishRule(PublishRule{ID: "pub_2", SandboxID: "sb2", UserID: "usr_a", Port: 3000, Alias: "app2"}); err != nil {
		t.Fatalf("different sandbox, same port should be OK: %v", err)
	}
}

func TestGetPublishRuleByAliasMissing(t *testing.T) {
	s := testStore(t)
	if _, err := s.GetPublishRuleByAlias("nonexistent"); err == nil {
		t.Fatal("expected error for missing alias")
	}
}

func TestListPublishRules(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestSandbox(t, s, "usr_a", "sb1", "app1")

	for i := 0; i < 3; i++ {
		s.CreatePublishRule(PublishRule{
			ID: fmt.Sprintf("pub_%d", i), SandboxID: "sb1", UserID: "usr_a",
			Port: 3000 + i, Alias: fmt.Sprintf("alias-%d", i),
		})
	}

	rules, err := s.ListPublishRules("sb1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 {
		t.Fatalf("expected 3 rules, got %d", len(rules))
	}
}

func TestDeletePublishRule(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestSandbox(t, s, "usr_a", "sb1", "app1")

	s.CreatePublishRule(PublishRule{ID: "pub_1", SandboxID: "sb1", UserID: "usr_a", Port: 3000, Alias: "my-app"})

	if err := s.DeletePublishRule("usr_a", "sb1", 3000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPublishRuleByAlias("my-app"); err == nil {
		t.Fatal("expected rule to be deleted")
	}
}

func TestDeletePublishRulesForSandbox(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestSandbox(t, s, "usr_a", "sb1", "app1")

	for i := 0; i < 3; i++ {
		s.CreatePublishRule(PublishRule{
			ID: fmt.Sprintf("pub_%d", i), SandboxID: "sb1", UserID: "usr_a",
			Port: 3000 + i, Alias: fmt.Sprintf("a-%d", i),
		})
	}

	n, err := s.DeletePublishRulesForSandbox("sb1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("expected 3 deleted, got %d", n)
	}
}

func TestDeletePreservesOtherSandbox(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestSandbox(t, s, "usr_a", "sb1", "app1")
	createTestSandbox(t, s, "usr_a", "sb2", "app2")

	s.CreatePublishRule(PublishRule{ID: "pub_a", SandboxID: "sb1", UserID: "usr_a", Port: 3000, Alias: "a"})
	s.CreatePublishRule(PublishRule{ID: "pub_b", SandboxID: "sb2", UserID: "usr_a", Port: 3000, Alias: "b"})

	s.DeletePublishRulesForSandbox("sb1")

	rules, _ := s.ListPublishRules("sb2")
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule for sb2, got %d", len(rules))
	}
}

func TestCleanupOrphanedPublishRules(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestSandbox(t, s, "usr_a", "sb1", "app1")
	createTestSandbox(t, s, "usr_a", "sb2", "app2")

	s.CreatePublishRule(PublishRule{ID: "pub_a", SandboxID: "sb1", UserID: "usr_a", Port: 3000, Alias: "alive"})
	s.CreatePublishRule(PublishRule{ID: "pub_b", SandboxID: "sb2", UserID: "usr_a", Port: 3000, Alias: "dead"})

	// Destroy sb2
	s.UpdateSandboxStatus("sb2", "destroyed")

	n, err := s.CleanupOrphanedPublishRules()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 orphaned rule cleaned, got %d", n)
	}

	// "alive" should still exist
	if _, err := s.GetPublishRuleByAlias("alive"); err != nil {
		t.Fatal("alive rule should still exist")
	}
	// "dead" should be gone
	if _, err := s.GetPublishRuleByAlias("dead"); err == nil {
		t.Fatal("dead rule should have been cleaned up")
	}
}

// --- Image sharing tests ---

func createTestImage(t *testing.T, s *Store, userID, name string) ImageRecord {
	t.Helper()
	img := ImageRecord{
		ID: fmt.Sprintf("img_%s_%s", userID, name), UserID: userID, Name: name,
		Source: "test", FilePath: "/tmp/" + name + ".ext4", SizeMB: 100,
		CreatedAt: time.Now(),
	}
	if err := s.CreateImage(img); err != nil {
		t.Fatal(err)
	}
	return img
}
