package server

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/broker"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// cloneEngine is the mock engine with what cloning needs: fork, checkpoint,
// resume and the snapshot archive. Each derives the clone's VM shape from the
// sandbox it came from the way the real engine does — the manifest carries
// vCPUs, RAM and the network policy, nothing about the image or the disk.
type cloneEngine struct {
	*portingEngine
	mu    sync.Mutex
	specs map[string]engine.SandboxSpec // engine ID → what the VM was created with

	// exported is the archive the last export produced: all the engine is
	// handed, which is the snapshot directory and the manifest.
	exported []byte
}

type cloneArchive struct {
	Manifest json.RawMessage `json:"manifest"`
	Files    []string        `json:"files"`
}

func (c *cloneEngine) remember(id string, spec engine.SandboxSpec) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.specs[id] = spec
}

func (c *cloneEngine) specOf(id string) (engine.SandboxSpec, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	spec, ok := c.specs[id]
	return spec, ok
}

func (c *cloneEngine) Create(ctx context.Context, spec engine.SandboxSpec) (engine.SandboxInfo, error) {
	info, err := c.mockEngine.Create(ctx, spec)
	if err == nil {
		c.remember(info.EngineID, spec)
	}
	return info, err
}

func (c *cloneEngine) Fork(ctx context.Context, id, newName string) (engine.SandboxInfo, error) {
	spec, ok := c.specOf(id)
	if !ok {
		return engine.SandboxInfo{}, fmt.Errorf("sandbox %q not found", id)
	}
	info, err := c.mockEngine.Create(ctx, engine.SandboxSpec{Name: newName})
	if err != nil {
		return info, err
	}
	spec.Name = newName
	c.remember(info.EngineID, spec)
	return info, nil
}

func (c *cloneEngine) Checkpoint(ctx context.Context, id, userID string, subnet int, name, dir string) (any, error) {
	return c.CheckpointTyped(ctx, id, userID, subnet, name, dir, "memory")
}

func (c *cloneEngine) CheckpointTyped(_ context.Context, id, _ string, _ int, name, dir, typ string) (any, error) {
	spec, ok := c.specOf(id)
	if !ok {
		return nil, fmt.Errorf("sandbox %q not found", id)
	}
	if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, name, "memory.bin"), make([]byte, 1<<20), 0o600); err != nil {
		return nil, err
	}
	vcpus := 1
	if spec.CPUs >= 1 {
		vcpus = int(spec.CPUs)
	}
	m := map[string]any{"type": typ, "vcpus": vcpus, "mem_mib": spec.MemoryMB, "token": "guest-token"}
	if typ == "memory" && spec.NetPolicy != nil {
		m["net_policy"] = spec.NetPolicy
	}
	return m, nil
}

func (c *cloneEngine) ResumeFromManifestJSON(ctx context.Context, dir string, manifest []byte, name, owner string) (engine.SandboxInfo, error) {
	info, err := c.portingEngine.ResumeFromManifestJSON(ctx, dir, manifest, name, owner)
	if err != nil {
		return info, err
	}
	var m struct {
		Vcpus     uint8                  `json:"vcpus"`
		MemMiB    int                    `json:"mem_mib"`
		NetPolicy *gateway.NetPolicyWire `json:"net_policy"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		return info, err
	}
	c.remember(info.EngineID, engine.SandboxSpec{Name: name, CPUs: float64(m.Vcpus), MemoryMB: m.MemMiB, NetPolicy: m.NetPolicy})
	return info, nil
}

func (c *cloneEngine) ExportSnapshot(_ context.Context, w io.Writer, dir string, manifest []byte, _ engine.SnapshotExport) error {
	arc := cloneArchive{Manifest: manifest}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		arc.Files = append(arc.Files, e.Name())
	}
	c.exported, err = json.Marshal(arc)
	if err != nil {
		return err
	}
	_, err = w.Write(c.exported)
	return err
}

func (c *cloneEngine) ImportSnapshot(_ context.Context, r io.Reader, dest string) (engine.SnapshotImport, error) {
	var arc cloneArchive
	if err := json.NewDecoder(r).Decode(&arc); err != nil {
		return engine.SnapshotImport{}, fmt.Errorf("%w: %v", engine.ErrBadSnapshotArchive, err)
	}
	if err := os.Mkdir(dest, 0o700); err != nil {
		return engine.SnapshotImport{}, err
	}
	for _, name := range arc.Files {
		if err := os.WriteFile(filepath.Join(dest, name), []byte("x"), 0o600); err != nil {
			return engine.SnapshotImport{}, err
		}
	}
	return engine.SnapshotImport{Name: "imported", Type: "memory", ManifestJSON: arc.Manifest}, nil
}

// setupClone is a server whose user may have sandboxes up to the given caps.
func setupClone(t *testing.T, maxCPUs, maxMemoryMB int) (*Server, *httptest.Server, *cloneEngine) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser(store.User{
		ID: "usr_test", Name: "test-user", APIKeyHash: sha256Hex(testAPIKey),
		MaxSandboxes: 50, MaxCPUsPerSandbox: maxCPUs, MaxMemoryMBPerSandbox: maxMemoryMB,
		SubnetIndex: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	eng := &cloneEngine{portingEngine: &portingEngine{mockEngine: newMockEngine()}, specs: map[string]engine.SandboxSpec{}}
	srv := New(eng, st, dir)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { srv.Close(); ts.Close(); st.Close() })
	expect(t, doReq(t, ts, "POST", "/secrets", map[string]string{"name": "GH_TOKEN", "value": "ghp_real"}), 201, nil)
	if err := st.CreateImage(store.ImageRecord{ID: "img-browser", UserID: "usr_test", Name: "browser",
		FilePath: "/images/browser.ext4", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return srv, ts, eng
}

// clonedJSON is a sandbox as the API shows it, with what a clone must keep.
type clonedJSON struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	EngineID     string      `json:"engine_id"`
	CPUs         float64     `json:"cpus"`
	MemoryMB     int         `json:"memory_mb"`
	DiskSizeMB   int         `json:"disk_size_mb"`
	Image        string      `json:"image"`
	HasInit      bool        `json:"has_init"`
	SecretGrants []grantJSON `json:"secret_grants"`
}

type shape struct {
	CPUs       float64
	MemoryMB   int
	DiskSizeMB int
	Image      string
	HasInit    bool
}

func (c clonedJSON) shape() shape { return shape{c.CPUs, c.MemoryMB, c.DiskSizeMB, c.Image, c.HasInit} }

// browserShape is a big browser sandbox, nothing like the defaults a record
// shows when it knows nothing (1 vCPU, 1 GB, minimal).
var browserShape = shape{CPUs: 4, MemoryMB: 8192, DiskSizeMB: 20480, Image: "browser", HasInit: true}

func createBrowser(t *testing.T, ts *httptest.Server, name string, extra map[string]any) clonedJSON {
	t.Helper()
	body := map[string]any{"name": name, "image": "browser", "cpus": 4, "memory_mb": 8192, "disk_size_mb": 20480,
		"init": "serve-app", "net_policy": map[string]any{"default": "public"}}
	for k, v := range extra {
		body[k] = v
	}
	var sb clonedJSON
	expect(t, doReq(t, ts, "POST", "/sandboxes", body), 201, &sb)
	return sb
}

var ghGrant = []map[string]any{{"secret": "GH_TOKEN", "hosts": []string{"api.github.com"}}}

func getSandbox(t *testing.T, ts *httptest.Server, name string) clonedJSON {
	t.Helper()
	var sb clonedJSON
	expect(t, doReq(t, ts, "GET", "/sandboxes/"+name, nil), 200, &sb)
	return sb
}

// netdBroker is the credential broker as netd reaches it: the daemon's own
// store and decryption behind a unix socket, asked over the wire.
func netdBroker(t *testing.T, srv *Server) *gateway.BrokerClient {
	t.Helper()
	dir, err := os.MkdirTemp("", "brk") // short: AF_UNIX paths are capped
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "b.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	b := broker.New(broker.Config{Store: srv.store, Decrypt: srv.decryptSecret})
	served := make(chan struct{})
	go func() { defer close(served); b.Serve(ln, "usr_test") }()
	t.Cleanup(func() { ln.Close(); <-served; os.RemoveAll(dir) })
	return &gateway.BrokerClient{Path: path}
}

// resolveAs asks the broker for the value behind placeholder, as netd does for
// the sandbox it knows by id; a refusal comes back as its reason.
func resolveAs(t *testing.T, c *gateway.BrokerClient, id, placeholder string) (value, refusal string) {
	t.Helper()
	resp, err := c.Do(context.Background(), gateway.BrokerRequest{Op: gateway.BrokerResolve, Sandbox: id, Placeholder: placeholder, Host: "api.github.com"})
	var r *gateway.BrokerRefusal
	switch {
	case err == nil:
		return resp.Value, ""
	case errors.As(err, &r):
		return "", r.Reason
	}
	t.Fatalf("broker: %v", err)
	return "", ""
}

func grantsOf(t *testing.T, ts *httptest.Server) []grantJSON {
	t.Helper()
	var list []grantJSON
	expect(t, doReq(t, ts, "GET", "/secrets/grants", nil), 200, &list)
	return list
}

// TestSnapshotResumeRecordsTheRestoredSpec: a sandbox resumed from a snapshot
// is recorded as the VM it is — vCPUs and RAM from the manifest, image, disk
// and boot config from the source's record when the snapshot was taken — even
// when the source is long gone. It used to read as the defaults.
func TestSnapshotResumeRecordsTheRestoredSpec(t *testing.T) {
	_, ts, _ := setupClone(t, 8, 16384)
	src := createBrowser(t, ts, "src", nil)
	if src.shape() != browserShape {
		t.Fatalf("control: the source is recorded as %+v", src.shape())
	}
	expect(t, doReq(t, ts, "POST", "/sandboxes/"+src.ID+"/checkpoint", map[string]any{"name": "base"}), 201, nil)
	expect(t, doReq(t, ts, "DELETE", "/sandboxes/"+src.ID, nil), 200, nil)

	var resumed clonedJSON
	expect(t, doReq(t, ts, "POST", "/snapshots/base/resume", map[string]any{"name": "clone"}), 201, &resumed)
	if resumed.shape() != browserShape {
		t.Fatalf("resume response records %+v, want %+v", resumed.shape(), browserShape)
	}
	if stored := getSandbox(t, ts, "clone"); stored.shape() != browserShape {
		t.Fatalf("stored record %+v, want %+v", stored.shape(), browserShape)
	}
}

// TestFilesystemSnapshotResumeRecordsTheSpecToo: a disk-only snapshot's
// manifest still names the VM's size, and the record keeps the image and disk.
func TestFilesystemSnapshotResumeRecordsTheSpecToo(t *testing.T) {
	srv, ts, _ := setupClone(t, 8, 16384)
	src := createBrowser(t, ts, "src", map[string]any{"secret_grants": ghGrant})
	expect(t, doReq(t, ts, "POST", "/sandboxes/"+src.ID+"/checkpoint", map[string]any{"name": "disk", "type": "filesystem"}), 201, nil)
	expect(t, doReq(t, ts, "DELETE", "/sandboxes/"+src.ID, nil), 200, nil)

	var resumed clonedJSON
	expect(t, doReq(t, ts, "POST", "/snapshots/disk/resume", map[string]any{"name": "clone"}), 201, &resumed)
	if resumed.shape() != browserShape {
		t.Fatalf("resume records %+v, want %+v", resumed.shape(), browserShape)
	}
	// It cold-boots a fresh guest: no CA in its trust store, no placeholders.
	if len(resumed.SecretGrants) != 0 {
		t.Fatalf("a filesystem resume got grants: %+v", resumed.SecretGrants)
	}
	if _, err := srv.store.GetSandboxCA(resumed.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a filesystem resume got a CA: %v", err)
	}
	if snap, err := srv.store.GetSnapshot("usr_test", "disk"); err != nil || snap.Substitution.CA != nil || len(snap.Substitution.Grants) != 0 {
		t.Fatalf("a filesystem snapshot kept substitution state: %+v, %v", snap, err)
	}

	// Even a record that holds a CA and grants gives a cold boot nothing: its
	// fresh guest has neither that CA in its trust store nor the placeholders.
	if err := srv.store.CreateSnapshot(store.SnapshotRecord{ID: "disk2", UserID: "usr_test", Name: "disk2",
		MemPath: "/x/disk2/mem.snap", ManifestJSON: `{"type":"filesystem","vcpus":1,"mem_mib":512}`, CreatedAt: time.Now(),
		Substitution: store.SnapshotSubstitution{
			CA:     &store.SandboxCA{CertPEM: "cert", KeyEnc: []byte("key")},
			Grants: []store.SnapshotGrant{{SecretName: "GH_TOKEN", Hosts: []string{"api.github.com"}, Placeholder: "bhatti_sec_dddddddddddddddddddddddd"}},
		}}); err != nil {
		t.Fatal(err)
	}
	var cold clonedJSON
	expect(t, doReq(t, ts, "POST", "/snapshots/disk2/resume", map[string]any{"name": "cold"}), 201, &cold)
	if held, err := srv.store.ListSandboxSecretGrants(cold.ID); err != nil || len(held) != 0 || len(cold.SecretGrants) != 0 {
		t.Fatalf("a cold boot inherited grants: %+v %+v, %v", cold.SecretGrants, held, err)
	}
	if _, err := srv.store.GetSandboxCA(cold.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a cold boot inherited a CA: %v", err)
	}
}

// TestSnapshotResumeHoldsTheCaps: resume is held to the same per-sandbox caps,
// with the same messages, as create; a snapshot exactly at them resumes, and a
// refusal creates nothing.
func TestSnapshotResumeHoldsTheCaps(t *testing.T) {
	srv, ts, eng := setupClone(t, 2, 4096)
	for name, manifest := range map[string]string{
		"big-cpu": `{"type":"memory","vcpus":4,"mem_mib":2048}`,
		"big-mem": `{"type":"memory","vcpus":1,"mem_mib":8192}`,
		"at-caps": `{"type":"memory","vcpus":2,"mem_mib":4096}`,
	} {
		if err := srv.store.CreateSnapshot(store.SnapshotRecord{ID: "id-" + name, UserID: "usr_test", Name: name,
			MemPath: "/x/" + name + "/mem.snap", ManifestJSON: manifest, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	refusedCreate := func(body map[string]any) string {
		t.Helper()
		resp := doReq(t, ts, "POST", "/sandboxes", body)
		if resp.StatusCode != 400 {
			t.Fatalf("control: create %v = %d, want 400", body, resp.StatusCode)
		}
		return errorBody(t, resp)
	}
	for snap, want := range map[string]string{
		"big-cpu": refusedCreate(map[string]any{"name": "c", "cpus": 4}),
		"big-mem": refusedCreate(map[string]any{"name": "m", "memory_mb": 8192}),
	} {
		resp := doReq(t, ts, "POST", "/snapshots/"+snap+"/resume", map[string]any{"name": "r-" + snap})
		if resp.StatusCode != 400 {
			t.Fatalf("resume of %s = %d, want 400", snap, resp.StatusCode)
		}
		if got := errorBody(t, resp); got != want {
			t.Fatalf("resume of %s says %q, create says %q", snap, got, want)
		}
	}
	if eng.resumed.snapDir != "" {
		t.Fatal("the engine resumed a VM over the caps")
	}
	if sbs, err := srv.store.ListSandboxes("usr_test"); err != nil || len(sbs) != 0 {
		t.Fatalf("refused resumes left sandboxes: %+v, %v", sbs, err)
	}

	var ok clonedJSON
	expect(t, doReq(t, ts, "POST", "/snapshots/at-caps/resume", map[string]any{"name": "fits"}), 201, &ok)
	if ok.CPUs != 2 || ok.MemoryMB != 4096 {
		t.Fatalf("a snapshot at the caps resumed as %+v", ok.shape())
	}
}

// TestForkRecordsTheSourceSpec: a fork is recorded as its source is — image,
// vCPUs, RAM, disk and boot config — whatever the request said.
func TestForkRecordsTheSourceSpec(t *testing.T) {
	_, ts, _ := setupClone(t, 8, 16384)
	createBrowser(t, ts, "src", nil)
	var child clonedJSON
	expect(t, doReq(t, ts, "POST", "/sandboxes", map[string]any{"name": "child", "from": "src", "cpus": 1, "disk_size_mb": 512}), 201, &child)
	if child.shape() != browserShape {
		t.Fatalf("fork response records %+v, want %+v", child.shape(), browserShape)
	}
	if stored := getSandbox(t, ts, "child"); stored.shape() != browserShape {
		t.Fatalf("stored fork %+v, want %+v", stored.shape(), browserShape)
	}
}

// TestForkInheritsGrants: a fork holds its source's placeholder in memory, so
// it gets a grant of its own for it — shown like any grant, resolving for the
// fork alone, revocable alone; deleting the secret takes every copy.
func TestForkInheritsGrants(t *testing.T) {
	srv, ts, _ := setupClone(t, 8, 16384)
	src := createBrowser(t, ts, "src", map[string]any{"secret_grants": ghGrant})
	ph := src.SecretGrants[0].Placeholder

	var child clonedJSON
	expect(t, doReq(t, ts, "POST", "/sandboxes", map[string]any{"name": "child", "from": "src"}), 201, &child)
	if len(child.SecretGrants) != 1 {
		t.Fatalf("the fork's grants: %+v", child.SecretGrants)
	}
	g := child.SecretGrants[0]
	if g.Placeholder != ph || g.SandboxID != child.ID || g.SandboxName != "child" || g.Status != "active" ||
		g.Secret != "GH_TOKEN" || !reflect.DeepEqual(g.Hosts, []string{"api.github.com"}) || g.ExpiresAt != nil ||
		g.ID == "" || g.ID == src.SecretGrants[0].ID {
		t.Fatalf("copy %+v of %+v", g, src.SecretGrants[0])
	}
	if listed := grantsOf(t, ts); len(listed) != 2 {
		t.Fatalf("grants listed: %+v, want the source's and the fork's", listed)
	}
	srcCA, err := srv.store.GetSandboxCA(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ca, err := srv.store.GetSandboxCA(child.ID); err != nil || ca.CertPEM != srcCA.CertPEM || !reflect.DeepEqual(ca.KeyEnc, srcCA.KeyEnc) {
		t.Fatalf("the fork's CA: %+v, %v; want its source's", ca, err)
	}

	// What netd's broker answers: the source is known by the server's ID, the
	// fork by the engine's. Only the fork and its source get the value.
	bc := netdBroker(t, srv)
	other := createBrowser(t, ts, "other", nil)
	for name, id := range map[string]string{"source": src.ID, "fork": child.EngineID} {
		if v, why := resolveAs(t, bc, id, ph); v != "ghp_real" {
			t.Fatalf("%s: value %q, refused %q", name, v, why)
		}
	}
	if v, why := resolveAs(t, bc, other.ID, ph); v != "" || why != "placeholder was granted to another sandbox" {
		t.Fatalf("an unrelated sandbox: value %q, refused %q", v, why)
	}

	// Revoking the fork's copy leaves the source's grant.
	expect(t, doReq(t, ts, "DELETE", "/secrets/grants/"+g.ID, nil), 200, nil)
	if v, why := resolveAs(t, bc, child.EngineID, ph); v != "" || why != "grant revoked" {
		t.Fatalf("the fork after revoking its grant: value %q, refused %q", v, why)
	}
	if v, why := resolveAs(t, bc, src.ID, ph); v != "ghp_real" {
		t.Fatalf("the source after the fork's revocation: value %q, refused %q", v, why)
	}

	expect(t, doReq(t, ts, "DELETE", "/secrets/GH_TOKEN", nil), 200, nil)
	if left := grantsOf(t, ts); len(left) != 0 {
		t.Fatalf("grants outlived their secret: %+v", left)
	}
}

// TestForkOnlyInheritsLiveGrants: a revoked or expired grant of the source
// isn't carried over.
func TestForkOnlyInheritsLiveGrants(t *testing.T) {
	srv, ts, _ := setupClone(t, 8, 16384)
	src := createBrowser(t, ts, "src", map[string]any{"secret_grants": ghGrant})
	var revoked grantJSON
	expect(t, doReq(t, ts, "POST", "/secrets/GH_TOKEN/grants", map[string]any{"sandbox": "src", "hosts": []string{"revoked.example"}}), 201, &revoked)
	expect(t, doReq(t, ts, "DELETE", "/secrets/grants/"+revoked.ID, nil), 200, nil)
	past := time.Now().Add(-time.Hour).UTC()
	if err := srv.store.CreateSecretGrant(store.SecretGrant{ID: "expired", UserID: "usr_test", SecretName: "GH_TOKEN", SandboxID: src.ID,
		Hosts: []string{"old.example"}, Placeholder: "bhatti_sec_aaaaaaaaaaaaaaaaaaaaaaaa", CreatedAt: past.Add(-time.Hour), ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}

	var child clonedJSON
	expect(t, doReq(t, ts, "POST", "/sandboxes", map[string]any{"name": "child", "from": "src"}), 201, &child)
	if len(child.SecretGrants) != 1 || child.SecretGrants[0].Placeholder != src.SecretGrants[0].Placeholder {
		t.Fatalf("the fork's grants: %+v, want only the live one", child.SecretGrants)
	}
	if held, err := srv.store.ListSandboxSecretGrants(child.ID); err != nil || len(held) != 1 {
		t.Fatalf("stored: %+v, %v", held, err)
	}
}

// TestForkRefusesNewGrantsAndSaysSourceGrantsAreInherited: a fork's
// environment is its source's, so a new placeholder couldn't reach it; the
// refusal says what it does get.
func TestForkRefusesNewGrantsAndSaysSourceGrantsAreInherited(t *testing.T) {
	_, ts, eng := setupClone(t, 8, 16384)
	createBrowser(t, ts, "src", map[string]any{"secret_grants": ghGrant})
	before := eng.nextID.Load()
	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{"name": "child", "from": "src", "secret_grants": ghGrant})
	if resp.StatusCode != 400 {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	if msg := errorBody(t, resp); !strings.Contains(msg, "can't be injected into a fork") || !strings.Contains(msg, "inherits the source's active grants") {
		t.Fatalf("message %q", msg)
	}
	if eng.nextID.Load() != before {
		t.Fatal("the refused fork reached the engine")
	}
}

// TestSnapshotResumeInheritsGrantsAfterTheSourceIsGone: a snapshot keeps the
// CA its guest trusts and the live grants of its source; every resume gets a
// copy of each, placeholder, hosts and expiry intact, though the source and its
// own grants and CA were destroyed. The copies work through the same broker
// lookups netd makes, independently of one another.
func TestSnapshotResumeInheritsGrantsAfterTheSourceIsGone(t *testing.T) {
	srv, ts, _ := setupClone(t, 8, 16384)
	src := createBrowser(t, ts, "src", map[string]any{"secret_grants": ghGrant})
	ph := src.SecretGrants[0].Placeholder
	var timed grantJSON // another grant of the source, with a lifetime
	expect(t, doReq(t, ts, "POST", "/secrets/GH_TOKEN/grants", map[string]any{"sandbox": "src", "hosts": []string{"uploads.github.com"}, "ttl": "24h"}), 201, &timed)
	var revoked grantJSON
	expect(t, doReq(t, ts, "POST", "/secrets/GH_TOKEN/grants", map[string]any{"sandbox": "src", "hosts": []string{"revoked.example"}}), 201, &revoked)
	expect(t, doReq(t, ts, "DELETE", "/secrets/grants/"+revoked.ID, nil), 200, nil)
	srcCA, err := srv.store.GetSandboxCA(src.ID)
	if err != nil {
		t.Fatal(err)
	}

	expect(t, doReq(t, ts, "POST", "/sandboxes/"+src.ID+"/checkpoint", map[string]any{"name": "base"}), 201, nil)
	expect(t, doReq(t, ts, "DELETE", "/sandboxes/"+src.ID, nil), 200, nil)
	if left := grantsOf(t, ts); len(left) != 0 {
		t.Fatalf("control: the destroyed source left grants %+v", left)
	}
	if _, err := srv.store.GetSandboxCA(src.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("control: the destroyed source left its CA: %v", err)
	}

	bc := netdBroker(t, srv)
	var clones [2]clonedJSON
	for i := range clones {
		expect(t, doReq(t, ts, "POST", "/snapshots/base/resume", map[string]any{"name": fmt.Sprintf("clone-%d", i)}), 201, &clones[i])
		c := clones[i]
		byPlaceholder := map[string]grantJSON{}
		for _, g := range c.SecretGrants {
			byPlaceholder[g.Placeholder] = g
			if g.SandboxID != c.ID || g.SandboxName != c.Name || g.Status != "active" || g.Secret != "GH_TOKEN" {
				t.Fatalf("%s: grant %+v", c.Name, g)
			}
		}
		if len(byPlaceholder) != 2 {
			t.Fatalf("%s: grants %+v, want the source's two live ones", c.Name, c.SecretGrants)
		}
		if g := byPlaceholder[ph]; !reflect.DeepEqual(g.Hosts, []string{"api.github.com"}) || g.ExpiresAt != nil {
			t.Fatalf("%s: grant for the guest's placeholder: %+v", c.Name, g)
		}
		if g := byPlaceholder[timed.Placeholder]; !reflect.DeepEqual(g.Hosts, []string{"uploads.github.com"}) ||
			g.ExpiresAt == nil || timed.ExpiresAt == nil || g.ExpiresAt.Sub(*timed.ExpiresAt).Abs() > time.Second {
			t.Fatalf("%s: timed grant %+v, want expiry %v", c.Name, g, timed.ExpiresAt)
		}
		if _, held := byPlaceholder[revoked.Placeholder]; held {
			t.Fatalf("%s inherited a revoked grant", c.Name)
		}

		// The CA the guest trusts, and a leaf signed by it for a granted host.
		ca, err := srv.store.GetSandboxCA(c.ID)
		if err != nil || ca.CertPEM != srcCA.CertPEM || !reflect.DeepEqual(ca.KeyEnc, srcCA.KeyEnc) {
			t.Fatalf("%s: CA %+v, %v; want the source's", c.Name, ca, err)
		}
		certResp, err := bc.Do(context.Background(), gateway.BrokerRequest{Op: gateway.BrokerCert, Sandbox: c.EngineID, Host: "api.github.com"})
		if err != nil {
			t.Fatalf("%s: no certificate for a granted host: %v", c.Name, err)
		}
		block, _ := pem.Decode([]byte(certResp.CertPEM))
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM([]byte(srcCA.CertPEM))
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "api.github.com", Roots: roots}); err != nil {
			t.Fatalf("%s: the leaf isn't signed by the CA its guest trusts: %v", c.Name, err)
		}

		// The placeholder in its memory works for it: through the broker's
		// per-sandbox lookup, netd knowing the clone by its engine ID.
		if v, why := resolveAs(t, bc, c.EngineID, ph); v != "ghp_real" {
			t.Fatalf("%s: value %q, refused %q", c.Name, v, why)
		}
		if _, why := resolveAs(t, bc, c.EngineID, revoked.Placeholder); why != "unknown placeholder" {
			t.Fatalf("%s: a revoked grant's placeholder: refused %q", c.Name, why)
		}
	}
	ids := map[string]bool{}
	for _, c := range clones {
		for _, g := range c.SecretGrants {
			ids[g.ID] = true
		}
	}
	if len(ids) != 4 {
		t.Fatalf("grant IDs %v: the two resumes must hold four grants of their own", ids)
	}
	if listed := grantsOf(t, ts); len(listed) != 4 {
		t.Fatalf("grants listed: %+v, want two per clone", listed)
	}

	// A resumed sandbox takes new grants: it has its CA.
	expect(t, doReq(t, ts, "POST", "/secrets/GH_TOKEN/grants", map[string]any{"sandbox": "clone-0", "hosts": []string{"api.github.com"}}), 201, nil)

	// Revoking one clone's copy leaves the other clone's.
	for _, g := range clones[0].SecretGrants {
		if g.Placeholder == ph {
			expect(t, doReq(t, ts, "DELETE", "/secrets/grants/"+g.ID, nil), 200, nil)
		}
	}
	if v, why := resolveAs(t, bc, clones[0].EngineID, ph); v != "" || why != "grant revoked" {
		t.Fatalf("clone-0 after revoking: value %q, refused %q", v, why)
	}
	if v, why := resolveAs(t, bc, clones[1].EngineID, ph); v != "ghp_real" {
		t.Fatalf("clone-1 after clone-0's revocation: value %q, refused %q", v, why)
	}
}

// TestSnapshotGrantsDoNotOutliveTheirSecret: deleting a secret takes the
// grants snapshots keep for it. A secret created again under the name — a
// different credential — isn't reachable through a resume, and a recorded
// grant whose secret is gone for any other reason is skipped.
func TestSnapshotGrantsDoNotOutliveTheirSecret(t *testing.T) {
	srv, ts, _ := setupClone(t, 8, 16384)
	src := createBrowser(t, ts, "src", map[string]any{"secret_grants": ghGrant})
	ph := src.SecretGrants[0].Placeholder
	expect(t, doReq(t, ts, "POST", "/sandboxes/"+src.ID+"/checkpoint", map[string]any{"name": "base"}), 201, nil)
	expect(t, doReq(t, ts, "DELETE", "/sandboxes/"+src.ID, nil), 200, nil)

	expect(t, doReq(t, ts, "DELETE", "/secrets/GH_TOKEN", nil), 200, nil)
	expect(t, doReq(t, ts, "POST", "/secrets", map[string]string{"name": "GH_TOKEN", "value": "a-different-credential"}), 201, nil)
	var clone clonedJSON
	expect(t, doReq(t, ts, "POST", "/snapshots/base/resume", map[string]any{"name": "clone"}), 201, &clone)
	if len(clone.SecretGrants) != 0 {
		t.Fatalf("a re-created secret came back through a resume: %+v", clone.SecretGrants)
	}
	bc := netdBroker(t, srv)
	if v, why := resolveAs(t, bc, clone.EngineID, ph); v != "" || why != "unknown placeholder" {
		t.Fatalf("value %q, refused %q", v, why)
	}

	// A kept grant whose secret vanished some other way is skipped; the rest are copied.
	if err := srv.store.CreateSnapshot(store.SnapshotRecord{ID: "kept", UserID: "usr_test", Name: "kept",
		MemPath: "/x/kept/mem.snap", ManifestJSON: `{"type":"memory"}`, CreatedAt: time.Now(),
		Substitution: store.SnapshotSubstitution{Grants: []store.SnapshotGrant{
			{SecretName: "GONE", Hosts: []string{"api.github.com"}, Placeholder: "bhatti_sec_bbbbbbbbbbbbbbbbbbbbbbbb"},
			{SecretName: "GH_TOKEN", Hosts: []string{"api.github.com"}, Placeholder: "bhatti_sec_cccccccccccccccccccccccc"},
		}}}); err != nil {
		t.Fatal(err)
	}
	var kept clonedJSON
	expect(t, doReq(t, ts, "POST", "/snapshots/kept/resume", map[string]any{"name": "kept-clone"}), 201, &kept)
	if len(kept.SecretGrants) != 1 || kept.SecretGrants[0].Secret != "GH_TOKEN" {
		t.Fatalf("grants of a resume whose snapshot kept a deleted secret's: %+v", kept.SecretGrants)
	}

	// And so is a fork's.
	src2 := createBrowser(t, ts, "src2", map[string]any{"secret_grants": ghGrant})
	if err := srv.store.DeleteSecret("usr_test", "GH_TOKEN"); err != nil {
		t.Fatal(err)
	}
	var child clonedJSON
	expect(t, doReq(t, ts, "POST", "/sandboxes", map[string]any{"name": "child", "from": src2.Name}), 201, &child)
	if len(child.SecretGrants) != 0 {
		t.Fatalf("a fork inherited a grant for a deleted secret: %+v", child.SecretGrants)
	}
	if _, err := srv.store.GetSandboxCA(child.ID); err != nil {
		t.Fatalf("the fork's CA: %v", err)
	}
}

// TestSnapshotExportAndImportCarryNoGrants: secrets are per host, so a
// snapshot archive holds neither grants nor the CA key; one imported (even back
// onto this host) resumes without them.
func TestSnapshotExportAndImportCarryNoGrants(t *testing.T) {
	srv, ts, eng := setupClone(t, 8, 16384)
	src := createBrowser(t, ts, "src", map[string]any{"secret_grants": ghGrant})
	ph := src.SecretGrants[0].Placeholder
	var snap store.SnapshotRecord
	expect(t, doReq(t, ts, "POST", "/sandboxes/"+src.ID+"/checkpoint", map[string]any{"name": "base"}), 201, &snap)
	stored, err := srv.store.GetSnapshot("usr_test", "base")
	if err != nil || len(stored.Substitution.Grants) != 1 || stored.Substitution.CA == nil {
		t.Fatalf("control: the snapshot keeps %+v, %v", stored.Substitution, err)
	}

	resp := doReq(t, ts, "GET", "/snapshots/base/export", nil)
	archive, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("export: %d %v", resp.StatusCode, err)
	}
	var arc cloneArchive
	if err := json.Unmarshal(archive, &arc); err != nil {
		t.Fatal(err)
	}
	if string(arc.Manifest) != stored.ManifestJSON {
		t.Fatalf("the engine was handed %s, not the snapshot's manifest %s", arc.Manifest, stored.ManifestJSON)
	}
	for _, secret := range []string{ph, stored.Substitution.CA.CertPEM, string(stored.Substitution.CA.KeyEnc), "GH_TOKEN", "api.github.com"} {
		if strings.Contains(string(eng.exported), secret) {
			t.Fatalf("the archive carries %q", secret)
		}
	}

	var imported store.SnapshotRecord
	expect(t, doReq(t, ts, "DELETE", "/snapshots/base", nil), 200, nil)
	req, _ := http.NewRequest("POST", ts.URL+"/snapshots/import?name=base", strings.NewReader(string(archive)))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	iresp, err := testHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, iresp, 201, &imported)
	back, err := srv.store.GetSnapshot("usr_test", "base")
	if err != nil || back.Substitution.CA != nil || len(back.Substitution.Grants) != 0 || back.Source != (store.SnapshotSource{}) {
		t.Fatalf("the imported snapshot kept %+v, %v", back, err)
	}
	var clone clonedJSON
	expect(t, doReq(t, ts, "POST", "/snapshots/base/resume", map[string]any{"name": "clone"}), 201, &clone)
	if len(clone.SecretGrants) != 0 {
		t.Fatalf("an imported snapshot resumed with grants: %+v", clone.SecretGrants)
	}
	if _, err := srv.store.GetSandboxCA(clone.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("an imported snapshot resumed with a CA: %v", err)
	}
	if held, err := srv.store.ListSandboxSecretGrants(clone.ID); err != nil || len(held) != 0 {
		t.Fatalf("grants of the resumed import: %+v, %v", held, err)
	}
}

// TestSnapshotAPIHidesWhatItKeeps: the CA key and the placeholders a snapshot
// keeps are daemon-side state; no snapshot endpoint shows them.
func TestSnapshotAPIHidesWhatItKeeps(t *testing.T) {
	srv, ts, _ := setupClone(t, 8, 16384)
	src := createBrowser(t, ts, "src", map[string]any{"secret_grants": ghGrant})
	body := func(method, path string, in any) string {
		t.Helper()
		resp := doReq(t, ts, method, path, in)
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode >= 300 {
			t.Fatalf("%s %s: %d %v %s", method, path, resp.StatusCode, err, b)
		}
		return string(b)
	}
	bodies := []string{
		body("POST", "/sandboxes/"+src.ID+"/checkpoint", map[string]any{"name": "base"}),
		body("GET", "/snapshots/base", nil),
		body("GET", "/snapshots", nil),
	}
	snap, err := srv.store.GetSnapshot("usr_test", "base")
	if err != nil || snap.Substitution.CA == nil || len(snap.Substitution.Grants) != 1 {
		t.Fatalf("control: the snapshot keeps %+v, %v", snap, err)
	}
	hidden := []string{src.SecretGrants[0].Placeholder, "GH_TOKEN", "api.github.com", "BEGIN CERTIFICATE",
		base64.StdEncoding.EncodeToString(snap.Substitution.CA.KeyEnc), "Substitution", "substitution"}
	for _, b := range bodies {
		for _, h := range hidden {
			if strings.Contains(b, h) {
				t.Fatalf("a snapshot endpoint shows %q: %s", h, b)
			}
		}
	}
}

// predatesRecording turns a snapshot into one taken before snapshots recorded
// their source: no record of its own, whatever CreateSnapshot wrote. It edits
// the database the way an older daemon left it.
func predatesRecording(t *testing.T, srv *Server, name string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(srv.dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id string
	if err := db.QueryRow(`SELECT id FROM snapshots WHERE user_id = 'usr_test' AND name = ?`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE snapshots SET source_image = '', source_disk_size_mb = 0, source_has_init = 0, source_recorded_at = NULL WHERE id = ?`,
		`DELETE FROM snapshot_cas WHERE snapshot_id = ?`,
		`DELETE FROM snapshot_grants WHERE snapshot_id = ?`,
	} {
		if _, err := db.Exec(stmt, id); err != nil {
			t.Fatal(err)
		}
	}
	if snap, err := srv.store.GetSnapshot("usr_test", name); err != nil || snap.SourceRecordedAt != nil ||
		snap.Source != (store.SnapshotSource{}) || snap.Substitution.CA != nil || len(snap.Substitution.Grants) != 0 {
		t.Fatalf("control: %q doesn't look like a snapshot from before they were recorded: %+v, %v", name, snap, err)
	}
}

// TestSnapshotFromBeforeRecordingResumesWithItsLiveSource: a snapshot a
// deployed daemon already holds, taken before snapshots recorded their source,
// resumes with the live source's CA, copies of its active grants as they are
// now, and its image, disk and boot config — so a clone can still be granted
// to afterwards. A disk-only one takes the same record but no CA or grants.
func TestSnapshotFromBeforeRecordingResumesWithItsLiveSource(t *testing.T) {
	srv, ts, _ := setupClone(t, 8, 16384)
	src := createBrowser(t, ts, "src", map[string]any{"secret_grants": ghGrant})
	ph := src.SecretGrants[0].Placeholder
	srcCA, err := srv.store.GetSandboxCA(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, doReq(t, ts, "POST", "/sandboxes/"+src.ID+"/checkpoint", map[string]any{"name": "base"}), 201, nil)
	expect(t, doReq(t, ts, "POST", "/sandboxes/"+src.ID+"/checkpoint", map[string]any{"name": "disk", "type": "filesystem"}), 201, nil)
	predatesRecording(t, srv, "base")
	predatesRecording(t, srv, "disk")
	var later grantJSON // the source has moved on since: its grants at resume time are what a clone gets
	expect(t, doReq(t, ts, "POST", "/secrets/GH_TOKEN/grants", map[string]any{"sandbox": "src", "hosts": []string{"uploads.github.com"}, "ttl": "24h"}), 201, &later)

	var clone clonedJSON
	expect(t, doReq(t, ts, "POST", "/snapshots/base/resume", map[string]any{"name": "clone"}), 201, &clone)
	if clone.shape() != browserShape {
		t.Fatalf("the resume records %+v, want its manifest's size and the live source's image, disk and boot config: %+v", clone.shape(), browserShape)
	}
	byPlaceholder := map[string]grantJSON{}
	for _, g := range clone.SecretGrants {
		byPlaceholder[g.Placeholder] = g
		if g.SandboxID != clone.ID || g.SandboxName != "clone" || g.Status != "active" || g.Secret != "GH_TOKEN" {
			t.Fatalf("grant %+v", g)
		}
	}
	if len(byPlaceholder) != 2 || byPlaceholder[ph].ID == "" || byPlaceholder[later.Placeholder].ID == "" {
		t.Fatalf("the clone's grants %+v, want copies of the source's two", clone.SecretGrants)
	}
	if ca, err := srv.store.GetSandboxCA(clone.ID); err != nil || ca.CertPEM != srcCA.CertPEM || !reflect.DeepEqual(ca.KeyEnc, srcCA.KeyEnc) {
		t.Fatalf("the clone's CA: %+v, %v; want the live source's", ca, err)
	}
	bc := netdBroker(t, srv)
	if v, why := resolveAs(t, bc, clone.EngineID, ph); v != "ghp_real" {
		t.Fatalf("the clone: value %q, refused %q", v, why)
	}
	if v, why := resolveAs(t, bc, src.ID, ph); v != "ghp_real" {
		t.Fatalf("the source: value %q, refused %q", v, why)
	}
	// Granting to the clone afterwards works: it has the CA.
	expect(t, doReq(t, ts, "POST", "/secrets/GH_TOKEN/grants", map[string]any{"sandbox": "clone", "hosts": []string{"api.github.com"}}), 201, nil)

	var cold clonedJSON
	expect(t, doReq(t, ts, "POST", "/snapshots/disk/resume", map[string]any{"name": "cold"}), 201, &cold)
	if cold.shape() != browserShape {
		t.Fatalf("the disk-only resume records %+v, want %+v", cold.shape(), browserShape)
	}
	if held, err := srv.store.ListSandboxSecretGrants(cold.ID); err != nil || len(held) != 0 || len(cold.SecretGrants) != 0 {
		t.Fatalf("a cold boot inherited grants: %+v, %v", held, err)
	}
	if _, err := srv.store.GetSandboxCA(cold.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a cold boot inherited a CA: %v", err)
	}
}

// TestSnapshotFromBeforeRecordingResumesAfterItsSourceIsGone: nothing was
// recorded and nothing is left to ask, so the resume just works — sized by its
// manifest, with no CA and no grants.
func TestSnapshotFromBeforeRecordingResumesAfterItsSourceIsGone(t *testing.T) {
	srv, ts, _ := setupClone(t, 8, 16384)
	src := createBrowser(t, ts, "src", map[string]any{"secret_grants": ghGrant})
	expect(t, doReq(t, ts, "POST", "/sandboxes/"+src.ID+"/checkpoint", map[string]any{"name": "base"}), 201, nil)
	predatesRecording(t, srv, "base")
	expect(t, doReq(t, ts, "DELETE", "/sandboxes/"+src.ID, nil), 200, nil)

	var clone clonedJSON
	expect(t, doReq(t, ts, "POST", "/snapshots/base/resume", map[string]any{"name": "clone"}), 201, &clone)
	if want := (shape{CPUs: 4, MemoryMB: 8192}); clone.shape() != want {
		t.Fatalf("the resume records %+v, want %+v", clone.shape(), want)
	}
	if held, err := srv.store.ListSandboxSecretGrants(clone.ID); err != nil || len(held) != 0 || len(clone.SecretGrants) != 0 {
		t.Fatalf("grants from nowhere: %+v %+v, %v", clone.SecretGrants, held, err)
	}
	if _, err := srv.store.GetSandboxCA(clone.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a CA from nowhere: %v", err)
	}
}

// TestSnapshotFromBeforeRecordingIgnoresAnotherUsersSandbox: a record of
// "its source" that is somebody else's sandbox is no source at all.
func TestSnapshotFromBeforeRecordingIgnoresAnotherUsersSandbox(t *testing.T) {
	srv, ts, _ := setupClone(t, 8, 16384)
	now := time.Now()
	for _, err := range []error{
		srv.store.CreateSandbox(store.Sandbox{ID: "theirs", Name: "theirs", EngineID: "eng-theirs", Status: "running", CreatedBy: "usr_other",
			CreatedAt: now, Image: "browser", DiskSizeMB: 999, HasInit: true}),
		srv.store.PutSandboxCA(store.SandboxCA{SandboxID: "theirs", UserID: "usr_other", CertPEM: "cert", KeyEnc: []byte("key")}),
		srv.store.CreateSecretGrant(store.SecretGrant{ID: "theirs-g", UserID: "usr_other", SecretName: "GH_TOKEN", SandboxID: "theirs",
			Hosts: []string{"api.github.com"}, Placeholder: "bhatti_sec_eeeeeeeeeeeeeeeeeeeeeeee", CreatedAt: now}),
		srv.store.CreateSnapshot(store.SnapshotRecord{ID: "mine", UserID: "usr_test", Name: "mine", SourceSandbox: "theirs",
			MemPath: "/x/mine/mem.snap", ManifestJSON: `{"type":"memory","vcpus":1,"mem_mib":1024}`, CreatedAt: now}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	predatesRecording(t, srv, "mine")

	var clone clonedJSON
	expect(t, doReq(t, ts, "POST", "/snapshots/mine/resume", map[string]any{"name": "clone"}), 201, &clone)
	if want := (shape{CPUs: 1, MemoryMB: 1024}); clone.shape() != want {
		t.Fatalf("the resume records %+v, want %+v", clone.shape(), want)
	}
	if held, err := srv.store.ListSandboxSecretGrants(clone.ID); err != nil || len(held) != 0 || len(clone.SecretGrants) != 0 {
		t.Fatalf("another user's grants: %+v %+v, %v", clone.SecretGrants, held, err)
	}
	if _, err := srv.store.GetSandboxCA(clone.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("another user's CA: %v", err)
	}
}

// TestRecordedSnapshotNeverMixesInTheLiveSource: a snapshot taken since
// snapshots record their source uses its own record and nothing else, however
// little that record holds — kept empty by a snapshot that had nothing to keep,
// not an older one with nothing recorded — and whatever its source has gained.
func TestRecordedSnapshotNeverMixesInTheLiveSource(t *testing.T) {
	srv, ts, _ := setupClone(t, 8, 16384)

	// Taken with a CA and no grants; the source is granted a secret afterwards.
	src := createBrowser(t, ts, "src", nil)
	srcCA, err := srv.store.GetSandboxCA(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, doReq(t, ts, "POST", "/sandboxes/"+src.ID+"/checkpoint", map[string]any{"name": "base"}), 201, nil)
	expect(t, doReq(t, ts, "POST", "/secrets/GH_TOKEN/grants", map[string]any{"sandbox": "src", "hosts": []string{"api.github.com"}}), 201, nil)
	var clone clonedJSON
	expect(t, doReq(t, ts, "POST", "/snapshots/base/resume", map[string]any{"name": "clone"}), 201, &clone)
	if held, err := srv.store.ListSandboxSecretGrants(clone.ID); err != nil || len(held) != 0 || len(clone.SecretGrants) != 0 {
		t.Fatalf("a grant the source gained after the snapshot reached its clone: %+v %+v, %v", clone.SecretGrants, held, err)
	}
	if ca, err := srv.store.GetSandboxCA(clone.ID); err != nil || ca.CertPEM != srcCA.CertPEM {
		t.Fatalf("the recorded CA: %+v, %v", ca, err)
	}

	// A record that kept nothing at all, over a source that has a spec, a CA and grants.
	rich := createBrowser(t, ts, "rich", map[string]any{"secret_grants": ghGrant})
	if err := srv.store.CreateSnapshot(store.SnapshotRecord{ID: "bare", UserID: "usr_test", Name: "bare", SourceSandbox: rich.ID,
		MemPath: "/x/bare/mem.snap", ManifestJSON: `{"type":"memory","vcpus":2,"mem_mib":2048}`, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var bare clonedJSON
	expect(t, doReq(t, ts, "POST", "/snapshots/bare/resume", map[string]any{"name": "bare-clone"}), 201, &bare)
	if want := (shape{CPUs: 2, MemoryMB: 2048}); bare.shape() != want {
		t.Fatalf("the resume records %+v, want %+v: the live source's spec leaked in", bare.shape(), want)
	}
	if held, err := srv.store.ListSandboxSecretGrants(bare.ID); err != nil || len(held) != 0 || len(bare.SecretGrants) != 0 {
		t.Fatalf("the live source's grants leaked in: %+v %+v, %v", bare.SecretGrants, held, err)
	}
	if _, err := srv.store.GetSandboxCA(bare.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the live source's CA leaked in: %v", err)
	}
}
