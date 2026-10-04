//go:build krucible

package krucible

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/broker"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// credcheck is a guest-side HTTPS client for the credential-substitution
// gate. Arguments have $VARS expanded from the guest env, so the placeholders
// lohar put there are sent as-is (exec runs no shell).
//
//	credcheck https URL [-H "Name: value"]... [-d BODY]  → ISSUER <cn>, STATUS <code>, then the body
//	credcheck issuer HOST:PORT                           → ISSUER <cn> of whatever leaf is presented
const credcheckSrc = `package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	args := make([]string, len(os.Args))
	for i, a := range os.Args {
		args[i] = os.ExpandEnv(a)
	}
	switch args[1] {
	case "https":
		method, body := "GET", ""
		var hdrs []string
		for i := 3; i+1 < len(args); i += 2 {
			switch args[i] {
			case "-H":
				hdrs = append(hdrs, args[i+1])
			case "-d":
				method, body = "POST", args[i+1]
			}
		}
		req, err := http.NewRequest(method, args[2], strings.NewReader(body))
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		for _, h := range hdrs {
			k, v, _ := strings.Cut(h, ":")
			req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
		}
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		fmt.Println("ISSUER", resp.TLS.PeerCertificates[0].Issuer.CommonName)
		fmt.Println("STATUS", resp.StatusCode)
		os.Stdout.Write(b)
	case "issuer":
		c, err := tls.Dial("tcp", args[2], &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		defer c.Close()
		fmt.Println("ISSUER", c.ConnectionState().PeerCertificates[0].Issuer.CommonName)
	}
}
`

type eventLog struct {
	mu     sync.Mutex
	events []store.Event
}

func (l *eventLog) Record(e store.Event) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}

func (l *eventLog) has(typ, secret, reasonPart string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		r, _ := e.Meta["reason"].(string)
		if e.Type == typ && e.Meta["secret"] == secret && strings.Contains(r, reasonPart) {
			return true
		}
	}
	return false
}

// TestKrucibleCredentialSubstitution is the end-to-end gate for credential
// substitution, through a real guest, lohar, a confined netd, the broker and a
// public HTTPS echo service: the guest holds only placeholders; a request to
// the granted host with a placeholder in a header reaches the upstream with
// the real value; a placeholder in the URL or body is refused; a revoked
// grant is refused; a host without a grant isn't intercepted at all.
func TestKrucibleCredentialSubstitution(t *testing.T) {
	echo := os.Getenv("KRUCIBLE_ECHO_HOST") // an HTTPS service echoing request headers at /headers
	if echo == "" {
		echo = "httpbin.org"
	}
	repo := repoRoot(t)
	if !hasLibkrun() || !hasHypervisor() {
		t.Skip("needs libkrun and a hypervisor")
	}
	vmm, netd := filepath.Join(repo, "bhatti-vmm"), filepath.Join(repo, "bhatti-netd")
	for _, p := range []string{vmm, netd} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("%s not built; skipping", filepath.Base(p))
		}
	}
	ensureVMMSigned(t, vmm)
	root := buildBaseRootfs(t, repo)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module cc\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(credcheckSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(root, "bin", "credcheck"), ".")
	build.Dir = src
	build.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build credcheck: %v\n%s", err, out)
	}
	eng, err := New(Config{
		DataDir: t.TempDir(), SocketDir: shortSockDir(t), BaseRootfs: root,
		VMMBinary: vmm, LibDir: libDir(), BlockRoot: true, NetdBinary: netd,
		KernelImage: requireLeanKernel(t, repo),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The daemon's side: a store with the secret, the sandbox's CA and its
	// grants, and the broker serving it to the owner's netd.
	st, err := store.New(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	events := &eventLog{}
	eng.SetCredentialBroker(broker.New(broker.Config{
		Store:   st,
		Decrypt: func(b []byte) ([]byte, error) { return b, nil },
		Record:  events.Record,
	}))
	const user, sandbox = "u-cred", "sb-cred"
	rb := make([]byte, 12)
	rand.Read(rb)
	value := "e2e-" + hex.EncodeToString(rb)
	for name, v := range map[string]string{"TOKEN": value, "OLD_TOKEN": "revoked-" + value} {
		if err := st.SetSecret(user, name, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	caPEM, caKey, err := broker.NewSandboxCA("bhatti sandbox e2e", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSandboxCA(store.SandboxCA{SandboxID: sandbox, UserID: user, CertPEM: string(caPEM), KeyEnc: caKey}); err != nil {
		t.Fatal(err)
	}
	grant := func(id, secret string) string {
		ph, err := gateway.NewPlaceholder()
		if err != nil {
			t.Fatal(err)
		}
		if err := st.CreateSecretGrant(store.SecretGrant{ID: id, UserID: user, SecretName: secret, SandboxID: sandbox,
			Hosts: []string{echo}, Placeholder: ph, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		return ph
	}
	ph, oldPH := grant("g-live", "TOKEN"), grant("g-old", "OLD_TOKEN")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	info, err := eng.Create(ctx, engine.SandboxSpec{
		Name: "credsub", CPUs: 1, MemoryMB: 512, UserID: user, SandboxID: sandbox,
		CACert: string(caPEM), NetPolicy: &gateway.NetPolicyWire{Default: "public"},
		Env: map[string]string{"TOKEN": ph, "OLD_TOKEN": oldPH},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), info.ID) })

	run := func(args ...string) string {
		t.Helper()
		r, err := eng.Exec(ctx, info.ID, args)
		if err != nil {
			t.Fatalf("exec %v: %v", args, err)
		}
		return r.Stdout + r.Stderr
	}
	// lohar trusts the sandbox CA and points TLS clients at the bundle.
	if got := strings.TrimSpace(run("printenv", "SSL_CERT_FILE")); got != "/etc/bhatti/ca-bundle.crt" {
		t.Fatalf("SSL_CERT_FILE = %q", got)
	}
	if !strings.Contains(run("cat", "/etc/bhatti/ca-bundle.crt"), strings.TrimSpace(string(caPEM))) {
		t.Fatal("the CA bundle lacks the sandbox CA")
	}
	if got := strings.TrimSpace(run("printenv", "TOKEN")); got != ph {
		t.Fatalf("guest TOKEN = %q, want the placeholder", got)
	}

	// A placeholder in a header reaches the granted host as the real value.
	out := run("credcheck", "https", "https://"+echo+"/headers", "-H", "Authorization: Bearer $TOKEN")
	if !strings.Contains(out, "ISSUER bhatti sandbox e2e") || !strings.Contains(out, "STATUS 200") {
		t.Fatalf("granted host not intercepted / not answered:\n%s", out)
	}
	if !strings.Contains(out, "Bearer "+value) || strings.Contains(out, ph) {
		t.Fatalf("upstream didn't see the real value in place of the placeholder:\n%s", out)
	}
	if !events.has("secret.used", "TOKEN", "") {
		t.Fatal("no secret.used event")
	}

	// A placeholder anywhere but a header is refused before it leaves.
	for what, args := range map[string][]string{
		"request target": {"credcheck", "https", "https://" + echo + "/anything?token=$TOKEN"},
		"request body":   {"credcheck", "https", "https://" + echo + "/anything", "-d", `{"token":"$TOKEN"}`},
	} {
		out := run(args...)
		if !strings.Contains(out, "STATUS 403") || !strings.Contains(out, "placeholder in "+what) {
			t.Fatalf("placeholder in the %s not refused:\n%s", what, out)
		}
		if !events.has("secret.denied", "TOKEN", "placeholder in "+what) {
			t.Fatalf("no secret.denied event for the %s", what)
		}
	}

	// A revoked grant is refused on the next connection.
	if err := st.RevokeSecretGrant(user, "g-old", time.Now()); err != nil {
		t.Fatal(err)
	}
	out = run("credcheck", "https", "https://"+echo+"/headers", "-H", "Authorization: Bearer $OLD_TOKEN")
	if !strings.Contains(out, "STATUS 403") || !strings.Contains(out, "grant revoked") || strings.Contains(out, "revoked-"+value) {
		t.Fatalf("revoked grant not refused:\n%s", out)
	}
	if !events.has("secret.denied", "OLD_TOKEN", "grant revoked") {
		t.Fatal("no secret.denied event for the revoked grant")
	}

	// A host no grant names is spliced: the guest sees the real certificate.
	out = run("credcheck", "issuer", "example.com:443")
	if !strings.HasPrefix(out, "ISSUER ") || strings.Contains(out, "bhatti") {
		t.Fatalf("a host without a grant was intercepted (or unreachable):\n%s", out)
	}
}
