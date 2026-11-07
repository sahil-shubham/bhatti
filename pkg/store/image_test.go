package store

import (
	"testing"
)

func TestImageShareVisibility(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_owner", "owner")
	createTestUser(t, s, "usr_friend", "friend")

	img := createTestImage(t, s, "usr_owner", "golden")

	// friend can't see it yet
	if _, err := s.GetImage(t.Context(), "usr_friend", "golden"); err == nil {
		t.Fatal("friend should not see unshared image")
	}

	// Share it
	if err := s.ShareImage(t.Context(), img.ID, "usr_friend"); err != nil {
		t.Fatal(err)
	}

	// Now friend can see it
	got, err := s.GetImage(t.Context(), "usr_friend", "golden")
	if err != nil {
		t.Fatalf("friend should see shared image: %v", err)
	}
	if got.Name != "golden" {
		t.Fatalf("expected golden, got %q", got.Name)
	}
}

func TestImageShareIsolation(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_owner", "owner")
	createTestUser(t, s, "usr_friend", "friend")
	createTestUser(t, s, "usr_stranger", "stranger")

	img := createTestImage(t, s, "usr_owner", "private")
	s.ShareImage(t.Context(), img.ID, "usr_friend")

	// friend can see it
	if _, err := s.GetImage(t.Context(), "usr_friend", "private"); err != nil {
		t.Fatal("friend should see shared image")
	}

	// stranger cannot
	if _, err := s.GetImage(t.Context(), "usr_stranger", "private"); err == nil {
		t.Fatal("stranger should NOT see image shared only with friend")
	}
}

func TestImageUnshare(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_owner", "owner")
	createTestUser(t, s, "usr_friend", "friend")

	img := createTestImage(t, s, "usr_owner", "temp-share")
	s.ShareImage(t.Context(), img.ID, "usr_friend")

	// friend sees it
	if _, err := s.GetImage(t.Context(), "usr_friend", "temp-share"); err != nil {
		t.Fatal("should see shared image")
	}

	// Unshare
	s.UnshareImage(t.Context(), img.ID, "usr_friend")

	// friend no longer sees it
	if _, err := s.GetImage(t.Context(), "usr_friend", "temp-share"); err == nil {
		t.Fatal("should not see unshared image")
	}
}

func TestImageShareListImages(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_owner", "owner")
	createTestUser(t, s, "usr_friend", "friend")

	createTestImage(t, s, "usr_owner", "shared-img")
	createTestImage(t, s, "usr_friend", "own-img")
	createTestImage(t, s, "", "admin-img")

	img, _ := s.GetImageByName(t.Context(), "shared-img")
	s.ShareImage(t.Context(), img.ID, "usr_friend")

	// friend should see: own-img + shared-img + admin-img
	list, err := s.ListImages(t.Context(), "usr_friend")
	if err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	for _, i := range list {
		names[i.Name] = true
	}
	for _, want := range []string{"own-img", "shared-img", "admin-img"} {
		if !names[want] {
			t.Errorf("missing %q in list (got %v)", want, names)
		}
	}
}

func TestListImageShares(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_owner", "owner")
	createTestUser(t, s, "usr_a", "alice")
	createTestUser(t, s, "usr_b", "bob")

	img := createTestImage(t, s, "usr_owner", "team-img")
	s.ShareImage(t.Context(), img.ID, "usr_a")
	s.ShareImage(t.Context(), img.ID, "usr_b")

	shares, err := s.ListImageShares(t.Context(), img.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 2 {
		t.Fatalf("expected 2 shares, got %d: %v", len(shares), shares)
	}
}

func TestGetImageByName(t *testing.T) {
	s := testStore(t)
	createTestImage(t, s, "usr_x", "findme")

	img, err := s.GetImageByName(t.Context(), "findme")
	if err != nil {
		t.Fatal(err)
	}
	if img.Name != "findme" {
		t.Fatalf("expected findme, got %q", img.Name)
	}

	_, err = s.GetImageByName(t.Context(), "nonexistent")
	if err == nil {
		t.Fatal("should error on nonexistent image")
	}
}

func TestGetUserByName(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_test", "testuser")

	u, err := s.GetUserByName(t.Context(), "testuser")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "usr_test" {
		t.Fatalf("expected usr_test, got %q", u.ID)
	}

	_, err = s.GetUserByName(t.Context(), "nobody")
	if err == nil {
		t.Fatal("should error on nonexistent user")
	}
}
