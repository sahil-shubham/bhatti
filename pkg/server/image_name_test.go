package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

func imageGateHeld(s *Server, userID, name string) bool {
	s.imageMu.Lock()
	defer s.imageMu.Unlock()
	gate := s.imageGates[imageName{userID, name}]
	if gate == nil {
		return false
	}
	if gate.mu.TryLock() {
		gate.mu.Unlock()
		return false
	}
	return true
}

func assertImageGateHeld(t *testing.T, s *Server, userID, name string) {
	t.Helper()
	if !imageGateHeld(s, userID, name) {
		t.Error("image publication did not retain its owner/name gate")
	}
	if _, err := s.store.GetImage(t.Context(), userID, name); err != nil {
		t.Errorf("publication ran without its registered record: %v", err)
	}
}

func imageRequest(s *Server, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testAPIKey)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func awaitImageSignal[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("image lifecycle did not reach its next synchronized phase")
		var zero T
		return zero
	}
}

type deletingImageEngine struct {
	*imageFilesEngine
	final   string
	entered chan struct{}
	release chan struct{}
}

func (e *deletingImageEngine) RemoveImageFiles(ctx context.Context, path string) error {
	if path == e.final {
		close(e.entered)
		<-e.release
	}
	return e.imageFilesEngine.RemoveImageFiles(ctx, path)
}

func TestImageRegistrationPublicationAndDeleteSerialize(t *testing.T) {
	s, ts, e := imageLifecycleFixture(t)
	sb := createSandbox(t, ts, "source")
	final := filepath.Join(s.dataDir, "images", "usr_test", "saved.ext4")
	de := &deletingImageEngine{e, final, make(chan struct{}), make(chan struct{})}
	s.engine = de
	attempts := make(chan struct{}, 4)
	s.onImageLock = func(_, _ string) { attempts <- struct{}{} }
	var captures atomic.Int32
	e.save = func(_ context.Context, _, stage string) error {
		content := "A"
		if captures.Add(1) > 1 {
			content = "B"
		}
		for _, path := range []string{stage, imageTestSidecar(stage)} {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				return err
			}
		}
		return nil
	}
	enteredA, releaseA := make(chan struct{}), make(chan struct{})
	var publishes atomic.Int32
	e.beforePublish = func(_, _ string, _ bool) error {
		if publishes.Add(1) == 1 {
			close(enteredA)
			<-releaseA
		}
		return nil
	}
	// Cleanup releases barriers even when an assertion fails before the race ends.
	defer func() {
		select {
		case <-releaseA:
		default:
			close(releaseA)
		}
		select {
		case <-de.release:
		default:
			close(de.release)
		}
	}()
	save := func() <-chan *httptest.ResponseRecorder {
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- imageRequest(s, "POST", "/sandboxes/"+sb.ID+"/save-image", `{"name":"saved"}`) }()
		return done
	}
	a := save()
	awaitImageSignal(t, enteredA) // A has registered, but has not renamed its stage.
	// Without the production registration gate, force the review's exact
	// DELETE -> B publication -> late A rename, rather than rely on scheduling.
	registeredLocked := imageGateHeld(s, "usr_test", "saved")
	if registeredLocked {
		awaitImageSignal(t, attempts) // A's acquisition.
	}
	deleted := make(chan *httptest.ResponseRecorder, 1)
	go func() { deleted <- imageRequest(s, "DELETE", "/images/saved", "") }()
	awaitImageSignal(t, attempts) // DELETE has registered as a gate waiter.
	var b <-chan *httptest.ResponseRecorder
	if registeredLocked {
		close(releaseA)
		if got := awaitImageSignal(t, a); got.Code != http.StatusCreated {
			t.Fatalf("A status = %d: %s", got.Code, got.Body)
		}
		awaitImageSignal(t, de.entered) // DELETE removed A's row, not its files yet.
		b = save()
		awaitImageSignal(t, attempts)
		assertImageDeletionGateHeld(t, s)
		close(de.release)
	} else {
		close(de.release)
	}
	if got := awaitImageSignal(t, deleted); got.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d: %s", got.Code, got.Body)
	}
	if b == nil {
		b = save()
	}
	if got := awaitImageSignal(t, b); got.Code != http.StatusCreated {
		t.Fatalf("B status = %d: %s", got.Code, got.Body)
	}
	if !registeredLocked {
		close(releaseA)
		awaitImageSignal(t, a)
	}
	img, err := s.store.GetImage(t.Context(), "usr_test", "saved")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{img.FilePath, imageTestSidecar(img.FilePath)} {
		if data, err := os.ReadFile(path); err != nil || string(data) != "B" {
			t.Errorf("B's registered image was overwritten: %s = %q, %v", path, data, err)
		}
	}
	s.imageMu.Lock()
	defer s.imageMu.Unlock()
	if len(s.imageGates) != 0 {
		t.Errorf("completed lifecycles retained %d image gates", len(s.imageGates))
	}
}

func assertImageDeletionGateHeld(t *testing.T, s *Server) {
	t.Helper()
	if !imageGateHeld(s, "usr_test", "saved") {
		t.Error("DELETE released its gate before file deletion completed")
	}
}

func TestSaveImagePublicationFailureDeletesOnlyItsRecord(t *testing.T) {
	s, ts, e := imageLifecycleFixture(t)
	sb := createSandbox(t, ts, "source")
	e.save = func(_ context.Context, _, stage string) error {
		return os.WriteFile(stage, []byte("attempt"), 0o600)
	}
	e.beforePublish = func(_, final string, _ bool) error {
		assertImageGateHeld(t, s, "usr_test", "saved")
		// An out-of-band store writer replaces the row while publication fails.
		// Identity cleanup must remain safe even if the old row is already gone.
		if err := s.store.DeleteImage(t.Context(), "usr_test", "saved"); err != nil {
			return err
		}
		if err := s.store.CreateImage(t.Context(), store.ImageRecord{ID: "replacement", UserID: "usr_test", Name: "saved", FilePath: final}); err != nil {
			return err
		}
		return errors.New("publication failed after registration was replaced")
	}
	resp := imageRequest(s, "POST", "/sandboxes/"+sb.ID+"/save-image", `{"name":"saved"}`)
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("save status = %d: %s", resp.Code, resp.Body)
	}
	img, err := s.store.GetImage(t.Context(), "usr_test", "saved")
	if err != nil || img.ID != "replacement" {
		t.Fatalf("failed publisher deleted the replacement: %+v, %v", img, err)
	}
	assertImagePairGone(t, waitImageCleanup(t, e))
}
