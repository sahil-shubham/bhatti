package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

type refusingForkEngine struct {
	*mockEngine
	err  error
	info engine.SandboxInfo
}

func (m *refusingForkEngine) Fork(context.Context, string, string) (engine.SandboxInfo, error) {
	return m.info, m.err
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

func TestForkReportsOldGuestReseedAndFailedReseed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		info   engine.SandboxInfo
		err    error
		event  string
		status int
	}{
		{"old", engine.SandboxInfo{ID: "old-clone", EngineID: "old-clone", Status: "running", GuestReseedUnsupported: true}, nil, "guest.reseed_unsupported", http.StatusCreated},
		{"failed", engine.SandboxInfo{}, fmt.Errorf("fork: %w", engine.ErrGuestReseedFailed), "guest.reseed_failed", http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts := setup(t)
			src := createSandbox(t, ts, "reseed-src")
			srv.StartEventRecorder()
			events := srv.events.Subscribe(SubscriptionFilter{TypePrefix: "guest.reseed"})
			defer events.Cancel()
			srv.engine = &refusingForkEngine{mockEngine: srv.engine.(*mockEngine), info: tc.info, err: tc.err}
			resp := doReq(t, ts, http.MethodPost, "/sandboxes", map[string]any{"name": "reseed-fork", "from": src.ID})
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("fork HTTP status = %d, want %d", resp.StatusCode, tc.status)
			}
			select {
			case event := <-events.C:
				if event.Type != tc.event || event.UserID != "usr_test" {
					t.Fatalf("reseed event = %+v, want %s", event, tc.event)
				}
			case <-time.After(time.Second):
				t.Fatalf("fork did not record %s", tc.event)
			}
		})
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

// A pre-separation lohar puts stderr on STDOUT, even when the client asked
// for stderr alone. The server must relay that merged stream rather than
// filtering every frame away and appearing to hang.
type oldPipedEngine struct{ *mockEngine }

func (m *oldPipedEngine) PipedSession(_ context.Context, _ string, _ engine.PipedSpec) (*proto.SessionInfo, engine.PipedConn, error) {
	exit := proto.ExitPayload(0)
	return &proto.SessionInfo{SessionID: "old-piped"}, &fakePiped{frames: [][2][]byte{
		{{proto.STDOUT}, []byte("stdout and stderr merged")},
		{{proto.EXIT}, exit[:]},
	}}, nil
}

func (m *oldPipedEngine) PipedSessionAttach(context.Context, string, string, bool) (*proto.SessionInfo, engine.PipedConn, error) {
	return nil, nil, errors.New("not attached")
}

func TestOldAgentPipedStderrMergesWithoutHanging(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, "old-piped")
	old := srv.engine.(*mockEngine)
	old.GuestFeatureErr = engine.GuestAgentOutdated("piped_stderr")
	srv.engine = &oldPipedEngine{old}
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/sandboxes/" + sb.ID + "/exec/ws"
	header := http.Header{}
	header.Set("Authorization", "Bearer "+testAPIKey)
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := ws.WriteJSON(map[string]any{"cmd": []string{"echo", "hello"}, "streams": []string{"stderr"}}); err != nil {
		t.Fatal(err)
	}
	_, meta, err := ws.ReadMessage()
	if err != nil || !strings.Contains(string(meta), `"type":"session"`) {
		t.Fatalf("session metadata %q: %v", meta, err)
	}
	typ, payload, err := ws.ReadMessage()
	if err != nil || typ != websocket.BinaryMessage || string(payload) != "\x01stdout and stderr merged" {
		t.Fatalf("old piped output type=%d data=%q err=%v; want merged stdout frame", typ, payload, err)
	}
	_, exit, err := ws.ReadMessage()
	if err != nil || !strings.Contains(string(exit), `"type":"exit"`) {
		t.Fatalf("session exit %q: %v", exit, err)
	}
}
