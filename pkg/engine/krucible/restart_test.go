//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
	"github.com/sahil-shubham/bhatti/pkg/server"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// TestKrucibleDaemonRestartKeepsSandboxes is the restart a daemon goes through
// on an upgrade, `systemctl restart` or a cert-renewal hook, on real VMs:
// engine A shuts down the way the daemon does (Shutdown) and engine B starts
// over the same data dir. Nothing reboots: each guest keeps its boot id, its
// processes and its tmpfs; a paused one is adopted paused and resumes; the
// owner's netd, adopted too, keeps enforcing each guest's egress policy, which
// the engine still knows when it reboots the guest. A second restart keeps the
// helpers B had only adopted. Shutdown used to kill every helper, and recovery
// couldn't adopt a paused guest (its agent can't answer): it marked it stopped
// and left its helper running.
func TestKrucibleDaemonRestartKeepsSandboxes(t *testing.T) {
	dataDir := vmmDir(t)
	sockDir := shortSockDir(t)
	base := buildBaseRootfs(t, repoRoot(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cur := recoveryEngine(t, dataDir, sockDir, base) // the running "daemon"'s engine
	const owner = "restart-owner"
	var ids []string  // the sandboxes, in creation order
	var helpers []int // their helpers' pids, as created
	var netdPID int
	// Helpers and netd are detached by design: the last engine destroys the
	// sandboxes, and whatever a failed check left behind is killed — if it is
	// still what it was, since a pid can be reused.
	t.Cleanup(func() {
		for _, id := range ids {
			_ = cur.Destroy(context.Background(), id)
		}
		for i, pid := range helpers {
			killHelper(pid, filepath.Join(dataDir, "sandboxes", ids[i], "vmspec.json"))
		}
		if netdPID > 0 && isNetd(netdPID, filepath.Join(cur.netdDir("u:"+owner), "n.sock")) {
			_ = syscall.Kill(netdPID, syscall.SIGKILL)
		}
	})

	policy := &gateway.NetPolicyWire{Default: "deny", AllowCIDRs: []string{"1.1.1.1/32"}}
	create := func(name string, np *gateway.NetPolicyWire) string {
		t.Helper()
		info, err := cur.Create(ctx, engine.SandboxSpec{Name: name, CPUs: 1, MemoryMB: 512, UserID: owner, NetPolicy: np})
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		ids = append(ids, info.ID)
		helpers = append(helpers, helperPID(t, cur, info.ID))
		return info.ID
	}
	busy := create("busy", policy)
	warm := create("warm", nil)
	netdPID = cur.netds["u:"+owner].pid

	run := func(id string, argv ...string) string {
		t.Helper()
		r, err := cur.Exec(ctx, id, argv)
		if err != nil || r.ExitCode != 0 {
			t.Fatalf("exec %v: err=%v exit=%d stderr=%q", argv, err, r.ExitCode, r.Stderr)
		}
		return strings.TrimSpace(r.Stdout)
	}
	bootID := func(id string) string { return run(id, "cat", "/proc/sys/kernel/random/boot_id") }
	egressHeld := func(when string) {
		t.Helper()
		dials := func(addr string) bool {
			r, err := cur.Exec(ctx, busy, []string{"netcheck", "dial", addr})
			if err != nil {
				t.Fatalf("%s: exec dial %s: %v", when, addr, err)
			}
			return r.ExitCode == 0
		}
		if !dials("1.1.1.1:443") {
			t.Fatalf("%s: the allow-listed 1.1.1.1 is unreachable", when)
		}
		if dials("8.8.8.8:443") {
			t.Fatalf("%s: 8.8.8.8 reachable — the guest's egress policy isn't enforced", when)
		}
	}

	busyBoot, warmBoot := bootID(busy), bootID(warm)
	sleeper, _, err := cur.ExecDetached(ctx, busy, []string{"sleep", "3600"}, "/tmp/sleep.log")
	if err != nil {
		t.Fatalf("start a guest process: %v", err)
	}
	run(busy, "writeuid", "/tmp/marker") // tmpfs: gone if the guest reboots
	egressHeld("before the restart")
	if err := cur.Pause(ctx, warm); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	restart := func() {
		t.Helper()
		cur.Shutdown()
		for _, pid := range append([]int{netdPID}, helpers...) {
			if !pidAlive(pid) {
				t.Fatalf("pid %d died with the daemon (helpers %v, netd %d)", pid, helpers, netdPID)
			}
		}
		cur = recoveryEngine(t, dataDir, sockDir, base)
		for i, id := range ids {
			if got := helperPID(t, cur, id); got != helpers[i] {
				t.Fatalf("%s: helper pid %d after the restart, want %d adopted", id, got, helpers[i])
			}
		}
		if got := cur.netds["u:"+owner].pid; got != netdPID {
			t.Fatalf("netd pid %d after the restart, want %d adopted", got, netdPID)
		}
	}

	restart()
	if s, err := cur.Status(ctx, busy); err != nil || s.Status != "running" || cur.ThermalState(busy) != "hot" {
		t.Fatalf("busy after the restart: %+v (%v), thermal %q, want running and hot", s, err, cur.ThermalState(busy))
	}
	if got := bootID(busy); got != busyBoot {
		t.Fatalf("busy rebooted across the restart: boot id %s, was %s", got, busyBoot)
	}
	if got := run(busy, "cat", fmt.Sprintf("/proc/%d/cmdline", sleeper)); !strings.Contains(got, "sleep") {
		t.Fatalf("guest process %d gone after the restart (cmdline %q)", sleeper, got)
	}
	if got := run(busy, "cat", "/tmp/marker"); got != "1000" {
		t.Fatalf("tmpfs marker after the restart = %q, want 1000", got)
	}
	egressHeld("after the restart")

	if got := cur.ThermalState(warm); got != "warm" {
		t.Fatalf("paused sandbox adopted %q, want warm", got)
	}
	if err := cur.EnsureHot(ctx, warm); err != nil {
		t.Fatalf("resume the adopted paused sandbox: %v", err)
	}
	if got := bootID(warm); got != warmBoot {
		t.Fatalf("paused sandbox rebooted: boot id %s, was %s", got, warmBoot)
	}
	vm := cur.vms[warm]
	vm.mu.Lock()
	info, infoErr := vm.AgentInfo, vm.AgentInfoErr
	vm.mu.Unlock()
	if infoErr != nil || (!info.Legacy && info.Version == "") {
		t.Fatalf("guest agent unasked after resuming the adopted sandbox: %+v, %v", info, infoErr)
	}

	restart() // helpers the previous engine only adopted
	if bootID(busy) != busyBoot || bootID(warm) != warmBoot {
		t.Fatal("a guest rebooted across the second restart")
	}

	// A reboot by an engine that only adopted the sandbox keeps its egress
	// policy: the engine reads it back from state.json.
	if err := cur.Stop(ctx, busy); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := cur.Start(ctx, busy); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if bootID(busy) == busyBoot {
		t.Fatal("Stop/Start didn't reboot the guest")
	}
	egressHeld("after a reboot by the adopting daemon")
}

// TestKrucibleServerRestartKeepsSandboxes drives a restart through the whole
// daemon over a real VM: daemon A's HTTP API, store and public proxy serve a
// keep_hot sandbox's published port; A shuts down the way the daemon does
// (drain, then Server.Shutdown); daemon B comes up over the same data dir and
// database, reconciles the store and auto-wakes keep_hot sandboxes as the
// daemon does. The guest wasn't rebooted, and B's public proxy reaches the
// same server process in it.
func TestKrucibleServerRestartKeepsSandboxes(t *testing.T) {
	dataDir := vmmDir(t)
	sockDir := shortSockDir(t)
	base := buildBaseRootfs(t, repoRoot(t))
	dbDir := t.TempDir()
	const apiKey = "test-token"

	type daemon struct {
		srv *server.Server
		st  *store.Store
		ts  *httptest.Server
	}
	start := func() *daemon {
		t.Helper()
		eng := recoveryEngine(t, dataDir, sockDir, base)
		st, err := store.New(filepath.Join(dbDir, "state.db"))
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		srv := server.New(eng, st, dbDir, server.WithProxyZone("test.sh"), server.WithAPIHost("api.test.sh"))
		srv.SetPublicProxy(server.NewPublicProxyHandler(eng, st, srv.ResumeSem(), srv.TouchActivity, srv.EnsureHot))
		srv.RecoverSandboxes(context.Background())
		return &daemon{srv: srv, st: st, ts: httptest.NewServer(srv)}
	}
	stop := func(d *daemon) {
		d.ts.Close()
		d.srv.Shutdown("terminated")
		d.st.Close()
	}
	do := func(d *daemon, method, path string, body any) *http.Response {
		t.Helper()
		var br io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			br = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, d.ts.URL+path, br)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}
	expect := func(resp *http.Response, code int, v any) {
		t.Helper()
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != code {
			t.Fatalf("%s %s: %d, want %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, code, b)
		}
		if v != nil {
			if err := json.Unmarshal(b, v); err != nil {
				t.Fatalf("decode %s: %v", b, err)
			}
		}
	}
	bootID := func(d *daemon, id string) string {
		t.Helper()
		var res engine.ExecResult
		expect(do(d, "POST", "/sandboxes/"+id+"/exec", map[string]any{"cmd": []string{"cat", "/proc/sys/kernel/random/boot_id"}}), 200, &res)
		return strings.TrimSpace(res.Stdout)
	}
	published := func(d *daemon) string {
		t.Helper()
		client := &http.Client{Timeout: 3 * time.Second}
		var last string
		for deadline := time.Now().Add(25 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
			req, _ := http.NewRequest("GET", d.ts.URL+"/", nil)
			req.Host = "restart-pub.test.sh"
			resp, err := client.Do(req)
			if err != nil {
				last = err.Error()
				continue
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if last = string(b); strings.Contains(last, "hello-from-guest") {
				return last
			}
		}
		t.Fatalf("published port never answered through the public proxy (last: %q)", last)
		return ""
	}

	a := start()
	sum := sha256.Sum256([]byte(apiKey))
	if err := a.st.CreateUser(store.User{
		ID: "usr_restart", Name: "restart-user", APIKeyHash: hex.EncodeToString(sum[:]),
		MaxSandboxes: 10, MaxCPUsPerSandbox: 4, MaxMemoryMBPerSandbox: 4096,
		SubnetIndex: 7, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	var sb store.Sandbox
	expect(do(a, "POST", "/sandboxes", map[string]any{"name": "pub", "memory_mb": 512, "keep_hot": true}), 201, &sb)
	cur := a
	t.Cleanup(func() {
		do(cur, "DELETE", "/sandboxes/"+sb.ID, nil).Body.Close()
		cur.ts.Close()
		cur.srv.Close()
		cur.st.Close()
	})
	expect(do(a, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd": []string{"/bin/netcheck", "serve", "18080"}, "detach": true, "output_file": "/tmp/serve.log",
	}), 200, nil)
	expect(do(a, "POST", "/sandboxes/"+sb.ID+"/publish", map[string]any{"port": 18080, "alias": "restart-pub"}), 201, nil)
	published(a)
	boot := bootID(a, sb.ID)

	stop(a)
	b := start()
	cur = b
	// The daemon's keep_hot auto-wake: a no-op for a sandbox that is hot.
	if err := b.srv.EnsureHot(context.Background(), sb.EngineID); err != nil {
		t.Fatalf("keep_hot auto-wake: %v", err)
	}

	published(b)
	if got := bootID(b, sb.ID); got != boot {
		t.Fatalf("guest rebooted across the daemon restart: boot id %s, was %s", got, boot)
	}
	var after store.Sandbox
	expect(do(b, "GET", "/sandboxes/"+sb.ID, nil), 200, &after)
	if after.Status != "running" || !after.KeepHot {
		t.Fatalf("after the restart: status %q keep_hot %v, want running and kept hot", after.Status, after.KeepHot)
	}
}
