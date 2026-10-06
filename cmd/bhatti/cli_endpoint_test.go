package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg"
)

func cliEndpointFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BHATTI_CONFIG", path)
	t.Setenv("BHATTI_URL", "")
	t.Setenv("BHATTI_TOKEN", "")
	oldURL, oldToken, oldSocket := apiURL, apiToken, unixSocketPath
	t.Cleanup(func() { apiURL, apiToken, unixSocketPath = oldURL, oldToken, oldSocket })
	return path
}

func cliSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bhatti-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestCLIAbsentSocketRemainsLocal(t *testing.T) {
	dataDir := cliSocketDir(t)
	cliEndpointFixture(t, fmt.Sprintf("data_dir: %q\nauth_token: local-key\n", dataDir))
	if err := loadConfig(rootCmd); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dataDir, "api.sock")
	if unixSocketPath != sock || apiURL != "http://unix" || apiToken != "local-key" {
		t.Fatalf("absent socket endpoint: socket=%q URL=%q token=%q", unixSocketPath, apiURL, apiToken)
	}
	_, err := apiRequest("GET", "/sandboxes", nil)
	want := "no bhatti daemon at " + sock + ": is it running? To use a remote server: bhatti setup"
	if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "localhost:8080") {
		t.Fatalf("absent socket error = %v, want local socket hint without TCP fallback", err)
	}
	_, err = wsDialer().NetDialContext(t.Context(), "tcp", "unix")
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("absent websocket socket error = %v, want local socket hint", err)
	}
}

func TestCLISocketDialErrors(t *testing.T) {
	sock := filepath.Join(cliSocketDir(t), "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(sock)

	_, err = dialUnixSocket(t.Context(), sock)
	want := "no bhatti daemon at " + sock + ": is it running? To use a remote server: bhatti setup"
	if err == nil || err.Error() != want {
		t.Fatalf("refused socket error = %v, want %q", err, want)
	}

	// Permission checks on mode 0000 are ineffective when tests run as root.
	// A wrapped syscall error exercises the same classification on every host.
	got := socketDialError(sock, fmt.Errorf("dial: %w", syscall.EACCES))
	want = "permission denied on " + sock + ": add yourself to the bhatti group (then log in again) or use sudo"
	if got.Error() != want {
		t.Fatalf("permission error = %q, want %q", got, want)
	}
	if got := socketDialError(sock, errors.New("other dial failure")); got.Error() != "other dial failure" {
		t.Fatalf("unexpected socket failure rewritten: %v", got)
	}
}

func TestCLIConfiguredRemoteURLNotRedirectedToSocket(t *testing.T) {
	cliEndpointFixture(t, "api_url: https://configured.example\nauth_token: configured-key\n")
	unixSocketPath = "stale-socket"
	if err := loadConfig(rootCmd); err != nil {
		t.Fatal(err)
	}
	if unixSocketPath != "" || apiURL != "https://configured.example" || apiToken != "configured-key" {
		t.Fatalf("configured remote endpoint: socket=%q URL=%q token=%q", unixSocketPath, apiURL, apiToken)
	}
	t.Setenv("BHATTI_URL", "https://environment.example")
	if err := loadConfig(rootCmd); err != nil {
		t.Fatal(err)
	}
	if unixSocketPath != "" || apiURL != "https://environment.example" {
		t.Fatalf("environment override: socket=%q URL=%q", unixSocketPath, apiURL)
	}
	versionCmd.InheritedFlags()
	urlFlag := versionCmd.Flags().Lookup("url")
	oldURLFlag, oldChanged := urlFlag.Value.String(), urlFlag.Changed
	t.Cleanup(func() {
		urlFlag.Value.Set(oldURLFlag)
		urlFlag.Changed = oldChanged
	})
	if err := versionCmd.Flags().Set("url", "https://flag.example"); err != nil {
		t.Fatal(err)
	}
	if err := loadConfig(versionCmd); err != nil {
		t.Fatal(err)
	}
	if unixSocketPath != "" || apiURL != "https://flag.example" {
		t.Fatalf("flag override: socket=%q URL=%q", unixSocketPath, apiURL)
	}
}

func TestCLIConfigParseErrorStopsEndpointResolution(t *testing.T) {
	path := cliEndpointFixture(t, "api_url: [unclosed\n")
	err := rootCmd.PersistentPreRunE(rootCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "parse config "+path) {
		t.Fatalf("config parse error = %v, want original parse failure", err)
	}
}

func TestCLISetupSavesLocalSocketCredentials(t *testing.T) {
	sock := filepath.Join(cliSocketDir(t), "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sandboxes" || r.Header.Get("Authorization") != "Bearer local-key" {
			t.Errorf("setup probe: path=%q auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Write([]byte("[]"))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	t.Setenv("HOME", t.TempDir())
	t.Setenv("SUDO_USER", "")
	cliEndpointFixture(t, fmt.Sprintf("api_socket: %q\n", sock))
	urlFlag, tokenFlag := setupCmd.Flags().Lookup("url"), setupCmd.Flags().Lookup("token")
	oldURL, oldToken := urlFlag.Value.String(), tokenFlag.Value.String()
	oldURLChanged, oldTokenChanged := urlFlag.Changed, tokenFlag.Changed
	t.Cleanup(func() {
		urlFlag.Value.Set(oldURL)
		tokenFlag.Value.Set(oldToken)
		urlFlag.Changed, tokenFlag.Changed = oldURLChanged, oldTokenChanged
	})
	if err := setupCmd.Flags().Set("url", "unix://"+sock); err != nil {
		t.Fatal(err)
	}
	if err := setupCmd.Flags().Set("token", "local-key"); err != nil {
		t.Fatal(err)
	}
	if err := loadConfig(setupCmd); err != nil {
		t.Fatal(err)
	}
	if unixSocketPath != sock {
		t.Fatalf("setup flag redirected local socket: %q", unixSocketPath)
	}
	if err := setupCmd.RunE(setupCmd, nil); err != nil {
		t.Fatalf("setup local socket: %v", err)
	}
	cfgPath := filepath.Join(pkg.DefaultDataDir(), "config.yaml")
	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != "auth_token: local-key\n" {
		t.Fatalf("local setup saved %q, want token-only credentials", saved)
	}

	if err := setupCmd.Flags().Set("url", "unix://"+filepath.Join(cliSocketDir(t), "other.sock")); err != nil {
		t.Fatal(err)
	}
	if err := setupCmd.RunE(setupCmd, nil); err == nil || !strings.Contains(err.Error(), "cannot be saved") {
		t.Fatalf("custom socket setup = %v, want rejection before writing config", err)
	}
	savedAfter, err := os.ReadFile(cfgPath)
	if err != nil || string(savedAfter) != string(saved) {
		t.Fatalf("rejected setup changed config: %q (%v)", savedAfter, err)
	}
}

func TestCLIVersionDisplaysUnixSocketPath(t *testing.T) {
	sock := filepath.Join(cliSocketDir(t), "absent.sock")
	cliEndpointFixture(t, fmt.Sprintf("api_socket: %q\n", sock))
	if err := loadConfig(rootCmd); err != nil {
		t.Fatal(err)
	}
	oldVersion := version
	version = "dev" // Avoid external release lookup; the socket probe fails locally.
	t.Cleanup(func() { version = oldVersion })

	for _, tc := range []struct {
		name string
		json bool
		want string
	}{
		{"text", false, "api: unix://" + sock},
		{"json", true, "\"api\": \"unix://" + sock + "\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			versionCmd.InheritedFlags()
			jsonFlag := versionCmd.Flags().Lookup("json")
			oldJSON, oldChanged := jsonFlag.Value.String(), jsonFlag.Changed
			t.Cleanup(func() {
				jsonFlag.Value.Set(oldJSON)
				jsonFlag.Changed = oldChanged
			})
			if err := versionCmd.Flags().Set("json", fmt.Sprint(tc.json)); err != nil {
				t.Fatal(err)
			}
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			oldStdout := os.Stdout
			os.Stdout = w
			defer func() { os.Stdout = oldStdout }()
			versionCmd.Run(versionCmd, nil)
			w.Close()
			out, err := io.ReadAll(r)
			r.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), tc.want) || strings.Contains(string(out), "http://unix") {
				t.Fatalf("version endpoint output = %q, want %q", out, tc.want)
			}
		})
	}
}
