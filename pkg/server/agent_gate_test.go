package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

type refusingForkEngine struct {
	*mockEngine
	err error
}

func (m *refusingForkEngine) Fork(context.Context, string, string) (engine.SandboxInfo, error) {
	return engine.SandboxInfo{}, m.err
}

func TestOldAgentForkReturnsConflict(t *testing.T) {
	srv, ts := setup(t)
	src := createSandbox(t, ts, "old-source")
	srv.engine = &refusingForkEngine{mockEngine: srv.engine.(*mockEngine), err: fmt.Errorf("fork: %w", engine.GuestAgentOutdated("net_config"))}
	resp := doReq(t, ts, http.MethodPost, "/sandboxes", map[string]any{"name": "fork-from-old", "from": src.ID})
	if got := errorBody(t, resp); resp.StatusCode != http.StatusConflict || !strings.Contains(got, "older guest agent") || !strings.Contains(got, "recreate it") {
		t.Fatalf("fork: %d %q, want 409 with recreate guidance", resp.StatusCode, got)
	}
	if _, err := srv.store.GetActiveSandboxByName("usr_test", "fork-from-old"); err == nil {
		t.Fatal("failed fork left a sandbox record")
	}
}

func TestOldAgentSecretGrantReturnsConflictWithoutGrant(t *testing.T) {
	srv, ts, eng := grantSetup(t)
	sb := createWith(t, ts, map[string]any{"name": "old-agent", "net_policy": map[string]any{"default": "public"}})
	eng.GuestFeatureErr = engine.GuestAgentOutdated("sandbox_ca")
	resp := doReq(t, ts, http.MethodPost, "/secrets/GH_TOKEN/grants", map[string]any{"sandbox": sb.Name, "hosts": []string{"api.github.com"}})
	if got := errorBody(t, resp); resp.StatusCode != http.StatusConflict || !strings.Contains(got, "older guest agent") || !strings.Contains(got, "recreate it") {
		t.Fatalf("grant: %d %q, want 409 with recreate guidance", resp.StatusCode, got)
	}
	if grants, err := srv.store.ListSecretGrants("usr_test"); err != nil || len(grants) != 0 {
		t.Fatalf("refused grant persisted: %+v, %v", grants, err)
	}

	eng.GuestFeatureErr = errors.New("info probe timeout")
	resp = doReq(t, ts, http.MethodPost, "/secrets/GH_TOKEN/grants", map[string]any{"sandbox": sb.Name, "hosts": []string{"api.github.com"}})
	if got := errorBody(t, resp); resp.StatusCode == http.StatusConflict || strings.Contains(got, "older guest agent") {
		t.Fatalf("unknown capabilities mislabeled old: %d %q", resp.StatusCode, got)
	}
}

func TestOldAgentCreateWithGrantOrGrowthLeavesNoSandbox(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    map[string]any
		feature string
	}{
		{"grant", map[string]any{"name": "old-grant", "net_policy": map[string]any{"default": "public"}, "secret_grants": []map[string]any{{"secret": "GH_TOKEN", "hosts": []string{"api.github.com"}}}}, "sandbox_ca"},
		{"disk-size", map[string]any{"name": "old-disk", "disk_size_mb": 2048}, "root_growth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts, eng := grantSetup(t)
			eng.CreateErr = engine.GuestAgentOutdated(tc.feature)
			resp := doReq(t, ts, http.MethodPost, "/sandboxes", tc.body)
			if got := errorBody(t, resp); resp.StatusCode != http.StatusConflict || !strings.Contains(got, "older guest agent") || !strings.Contains(got, "recreate it") {
				t.Fatalf("create: %d %q, want 409 with recreate guidance", resp.StatusCode, got)
			}
			if list, err := srv.store.ListSandboxes("usr_test"); err != nil || len(list) != 0 {
				t.Fatalf("refused sandbox persisted: %+v, %v", list, err)
			}
			if grants, err := srv.store.ListSecretGrants("usr_test"); err != nil || len(grants) != 0 {
				t.Fatalf("refused create left grants: %+v, %v", grants, err)
			}
		})
	}
}
