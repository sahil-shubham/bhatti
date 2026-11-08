package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

type imageFilesEngine struct {
	engine.Engine
	save          func(context.Context, string, string) error
	removed       chan string
	published     chan bool
	beforePublish func(string, string, bool) error
}

func imageTestSidecar(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".json")
}

func (e *imageFilesEngine) SaveImage(ctx context.Context, id, path string) error {
	return e.save(ctx, id, path)
}

func (e *imageFilesEngine) RemoveImageFiles(_ context.Context, path string) error {
	for _, p := range []string{imageTestSidecar(path), path} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	e.removed <- path
	return nil
}

func (e *imageFilesEngine) PublishRawImage(_ context.Context, staging, final string) error {
	if err := os.WriteFile(imageTestSidecar(staging), []byte(`{"format":"raw","backing":[]}`), 0o600); err != nil {
		return err
	}
	return e.publish(staging, final, true)
}

func (e *imageFilesEngine) PublishSavedImage(_ context.Context, staging, final string) error {
	return e.publish(staging, final, false)
}

func (e *imageFilesEngine) publish(staging, final string, raw bool) error {
	if e.beforePublish != nil {
		if err := e.beforePublish(staging, final, raw); err != nil {
			return err
		}
	}
	if err := os.Rename(staging, final); err != nil {
		return err
	}
	if err := os.Rename(imageTestSidecar(staging), imageTestSidecar(final)); err != nil {
		return err
	}
	e.published <- raw
	return nil
}

func imageLifecycleFixture(t *testing.T) (*Server, *httptest.Server, *imageFilesEngine) {
	t.Helper()
	s, ts := setup(t)
	e := &imageFilesEngine{Engine: s.engine, removed: make(chan string, 8), published: make(chan bool, 8)}
	s.engine = e
	s.loharPath = filepath.Join(t.TempDir(), "lohar")
	if err := os.WriteFile(s.loharPath, []byte("lohar fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "mke2fs"), []byte("#!/bin/sh\nexit \"${BHATTI_TEST_MKE2FS_EXIT:-0}\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return s, ts, e
}

func waitImageCleanup(t *testing.T, e *imageFilesEngine) string {
	t.Helper()
	select {
	case path := <-e.removed:
		return path
	case <-time.After(10 * time.Second):
		t.Fatal("image attempt did not finish paired cleanup")
		return ""
	}
}

func assertImagePairGone(t *testing.T, path string) {
	t.Helper()
	for _, p := range []string{path, imageTestSidecar(path)} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("attempt retained %s: %v", p, err)
		}
	}
}

func TestImageDeleteDelegatesPairedRemoval(t *testing.T) {
	s, ts, e := imageLifecycleFixture(t)
	path := filepath.Join(s.dataDir, "images", "usr_test", "saved.ext4")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, imageTestSidecar(path)} {
		if err := os.WriteFile(p, []byte("saved"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.store.CreateImage(t.Context(), store.ImageRecord{ID: "saved", UserID: "usr_test", Name: "saved", FilePath: path}); err != nil {
		t.Fatal(err)
	}
	resp := doReq(t, ts, "DELETE", "/images/saved", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	if removed := waitImageCleanup(t, e); removed != path {
		t.Fatalf("removed %s, want %s", removed, path)
	}
	assertImagePairGone(t, path)
}

func TestSaveImageFailureCleansOnlyItsStagedPair(t *testing.T) {
	for _, failure := range []string{"capture", "registration"} {
		t.Run(failure, func(t *testing.T) {
			s, ts, e := imageLifecycleFixture(t)
			sb := createSandbox(t, ts, "source")
			final := filepath.Join(s.dataDir, "images", "usr_test", "saved.ext4")
			e.save = func(_ context.Context, _ string, staging string) error {
				for _, p := range []string{staging, imageTestSidecar(staging)} {
					if err := os.WriteFile(p, []byte("attempt"), 0o600); err != nil {
						return err
					}
				}
				if failure == "capture" {
					return errors.New("capture failed after writing sidecar")
				}
				// Force a registration conflict without a timing race. A failed
				// attempt owns its stage, not the image another request saved.
				for _, p := range []string{final, imageTestSidecar(final)} {
					if err := os.WriteFile(p, []byte("winner"), 0o600); err != nil {
						return err
					}
				}
				return s.store.CreateImage(t.Context(), store.ImageRecord{ID: "winner", UserID: "usr_test", Name: "saved", FilePath: final})
			}
			resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/save-image", map[string]string{"name": "saved"})
			resp.Body.Close()
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("save status = %d", resp.StatusCode)
			}
			staging := waitImageCleanup(t, e)
			if staging == final {
				t.Fatal("failed attempt used the registered image's pathname")
			}
			assertImagePairGone(t, staging)
			if failure == "registration" {
				for _, p := range []string{final, imageTestSidecar(final)} {
					if data, err := os.ReadFile(p); err != nil || string(data) != "winner" {
						t.Fatalf("registration failure removed winner %s: %q, %v", p, data, err)
					}
				}
			}
		})
	}
}

func TestRawImagePublishersReplaceMetadataAndCleanFailures(t *testing.T) {
	for _, publisher := range []string{"import", "pull"} {
		for _, failure := range []string{"none", "conversion", "registration", "publication"} {
			t.Run(publisher+"/"+failure, func(t *testing.T) {
				s, ts, e := imageLifecycleFixture(t)
				e.beforePublish = func(_, _ string, _ bool) error {
					assertImageGateHeld(t, s, "usr_test", "raw")
					if failure == "publication" {
						return errors.New("injected image publication failure")
					}
					return nil
				}
				if failure == "conversion" {
					t.Setenv("BHATTI_TEST_MKE2FS_EXIT", "1")
				}
				if failure == "registration" {
					db, err := sql.Open("sqlite", filepath.Join(s.dataDir, "test.db"))
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					if _, err := db.Exec(`CREATE TRIGGER reject_image BEFORE INSERT ON images BEGIN SELECT RAISE(FAIL, 'injected image registration failure'); END`); err != nil {
						t.Fatal(err)
					}
				}
				final := filepath.Join(s.dataDir, "images", "usr_test", "raw.ext4")
				if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(imageTestSidecar(final), []byte(`{"format":"qcow2","backing":[]}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if publisher == "import" {
					archive := filepath.Join(t.TempDir(), "image.tar")
					tag, err := name.NewTag("fixture:latest")
					if err != nil {
						t.Fatal(err)
					}
					if err := tarball.WriteToFile(archive, tag, empty.Image); err != nil {
						t.Fatal(err)
					}
					body, err := os.ReadFile(archive)
					if err != nil {
						t.Fatal(err)
					}
					req, err := http.NewRequest("POST", ts.URL+"/images/import?name=raw", bytes.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Authorization", "Bearer "+testAPIKey)
					resp, err := testHTTPClient.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					data, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					want := http.StatusCreated
					if failure == "conversion" {
						want = http.StatusBadRequest
					} else if failure == "registration" || failure == "publication" {
						want = http.StatusInternalServerError
					}
					if resp.StatusCode != want {
						t.Fatalf("import status %d, want %d: %s", resp.StatusCode, want, data)
					}
				} else {
					reg := httptest.NewServer(registry.New())
					defer reg.Close()
					tag, err := name.NewTag(strings.TrimPrefix(reg.URL, "http://")+"/fixture:latest", name.Insecure)
					if err != nil {
						t.Fatal(err)
					}
					if err := remote.Write(tag, empty.Image); err != nil {
						t.Fatal(err)
					}
					resp := doReq(t, ts, "POST", "/images/pull", map[string]string{"ref": tag.Name(), "name": "raw"})
					resp.Body.Close()
					if resp.StatusCode != http.StatusAccepted {
						t.Fatalf("pull status %d", resp.StatusCode)
					}
				}
				staging := waitImageCleanup(t, e)
				assertImagePairGone(t, staging)
				if failure == "none" {
					select {
					case raw := <-e.published:
						if !raw {
							t.Fatal("raw converter selected saved-image publication")
						}
					default:
						t.Fatal("converter bypassed engine publication")
					}
					var metadata struct{ Format string }
					data, err := os.ReadFile(imageTestSidecar(final))
					if err != nil || json.Unmarshal(data, &metadata) != nil || metadata.Format != "raw" {
						t.Fatalf("raw publisher retained saved metadata: %s, %v", data, err)
					}
				} else {
					select {
					case <-e.published:
						t.Fatal("failed conversion/registration published an image")
					default:
					}
					if _, err := os.Stat(final); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("failed publisher left a final disk: %v", err)
					}
					if _, err := s.store.GetImage(t.Context(), "usr_test", "raw"); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("failed publisher retained its registration: %v", err)
					}
				}
			})
		}
	}
}
