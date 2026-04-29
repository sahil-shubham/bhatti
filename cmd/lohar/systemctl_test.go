//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseServiceFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.service")
	os.WriteFile(path, []byte(`[Unit]
Description=Test Service
After=network.target

[Service]
Type=simple
ExecStartPre=-/usr/bin/test-pre
ExecStart=/usr/bin/test-daemon -D $OPTS
ExecReload=/bin/kill -HUP $MAINPID
RuntimeDirectory=testd
RuntimeDirectoryMode=0750
EnvironmentFile=-/etc/default/test
Environment="FOO=bar"
User=testuser
WorkingDirectory=/opt/test

[Install]
WantedBy=multi-user.target
Alias=testd.service
`), 0644)

	svc := parseServiceFile(path)

	tests := []struct {
		section, key, want string
	}{
		{"Unit", "Description", "Test Service"},
		{"Unit", "After", "network.target"},
		{"Service", "Type", "simple"},
		{"Service", "ExecStart", "/usr/bin/test-daemon -D $OPTS"},
		{"Service", "ExecReload", "/bin/kill -HUP $MAINPID"},
		{"Service", "RuntimeDirectory", "testd"},
		{"Service", "RuntimeDirectoryMode", "0750"},
		{"Service", "User", "testuser"},
		{"Service", "WorkingDirectory", "/opt/test"},
		{"Install", "WantedBy", "multi-user.target"},
		{"Install", "Alias", "testd.service"},
	}
	for _, tt := range tests {
		got := svc.get(tt.section, tt.key)
		if got != tt.want {
			t.Errorf("get(%q, %q) = %q, want %q", tt.section, tt.key, got, tt.want)
		}
	}

	// getAll for multi-value keys
	pres := svc.getAll("Service", "ExecStartPre")
	if len(pres) != 1 || pres[0] != "-/usr/bin/test-pre" {
		t.Errorf("getAll ExecStartPre = %v, want [-/usr/bin/test-pre]", pres)
	}

	envs := svc.getAll("Service", "Environment")
	if len(envs) != 1 || envs[0] != `"FOO=bar"` {
		t.Errorf("getAll Environment = %v", envs)
	}

	// Missing key
	if got := svc.get("Service", "NoSuchKey"); got != "" {
		t.Errorf("missing key returned %q", got)
	}
}

func TestParseServiceFileMultipleExecStartPre(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "multi.service")
	os.WriteFile(path, []byte(`[Service]
ExecStartPre=-/usr/bin/first
ExecStartPre=/usr/bin/second
ExecStartPre=-/usr/bin/third
ExecStart=/usr/bin/main
`), 0644)

	svc := parseServiceFile(path)
	pres := svc.getAll("Service", "ExecStartPre")
	if len(pres) != 3 {
		t.Fatalf("expected 3 ExecStartPre, got %d: %v", len(pres), pres)
	}
	if pres[0] != "-/usr/bin/first" || pres[1] != "/usr/bin/second" || pres[2] != "-/usr/bin/third" {
		t.Errorf("ExecStartPre = %v", pres)
	}
}

func TestNormalizeName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ssh", "ssh"},
		{"ssh.service", "ssh"},
		{"nginx.service", "nginx"},
		{"nginx", "nginx"},
	}
	for _, tt := range tests {
		if got := normalizeName(tt.in); got != tt.want {
			t.Errorf("normalizeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIsTarget(t *testing.T) {
	if !isTarget("sysinit") {
		t.Error("sysinit should be a target")
	}
	if !isTarget("multi-user") {
		t.Error("multi-user should be a target")
	}
	if !isTarget("network-online") {
		t.Error("network-online should be a target")
	}
	if isTarget("ssh") {
		t.Error("ssh should not be a target")
	}
	if isTarget("nginx") {
		t.Error("nginx should not be a target")
	}
}

func TestSvcIsActiveTargets(t *testing.T) {
	// Well-known targets must always report active.
	if !svcIsActive("sysinit") {
		t.Error("sysinit.target should be active")
	}
	if !svcIsActive("multi-user") {
		t.Error("multi-user.target should be active")
	}
}

func TestFindServiceFile(t *testing.T) {
	dir := t.TempDir()

	// Override serviceDirs for this test.
	origDirs := serviceDirs
	serviceDirs = []string{dir}
	defer func() { serviceDirs = origDirs }()

	// Create a service file.
	svcPath := filepath.Join(dir, "test.service")
	os.WriteFile(svcPath, []byte("[Service]\nExecStart=/bin/true\n"), 0644)

	if got := findServiceFile("test"); got != svcPath {
		t.Errorf("findServiceFile(test) = %q, want %q", got, svcPath)
	}

	if got := findServiceFile("nonexistent"); got != "" {
		t.Errorf("findServiceFile(nonexistent) = %q, want empty", got)
	}
}

func TestFindServiceFileAlias(t *testing.T) {
	dir := t.TempDir()
	origDirs := serviceDirs
	serviceDirs = []string{dir}
	defer func() { serviceDirs = origDirs }()

	svcPath := filepath.Join(dir, "ssh.service")
	os.WriteFile(svcPath, []byte("[Service]\nExecStart=/usr/sbin/sshd\n[Install]\nAlias=sshd.service\n"), 0644)

	// Find by canonical name.
	if got := findServiceFile("ssh"); got != svcPath {
		t.Errorf("findServiceFile(ssh) = %q, want %q", got, svcPath)
	}
	// Find by alias.
	if got := findServiceFile("sshd"); got != svcPath {
		t.Errorf("findServiceFile(sshd) = %q, want %q", got, svcPath)
	}
}

func TestFindServiceFileMasked(t *testing.T) {
	dir := t.TempDir()
	origDirs := serviceDirs
	serviceDirs = []string{dir}
	defer func() { serviceDirs = origDirs }()

	svcPath := filepath.Join(dir, "masked.service")
	os.Symlink("/dev/null", svcPath)

	if got := findServiceFile("masked"); got != "" {
		t.Errorf("findServiceFile(masked) should return empty for masked, got %q", got)
	}
}

func TestIsMasked(t *testing.T) {
	dir := t.TempDir()
	origDirs := serviceDirs
	serviceDirs = []string{dir}
	defer func() { serviceDirs = origDirs }()

	os.Symlink("/dev/null", filepath.Join(dir, "masked.service"))
	os.WriteFile(filepath.Join(dir, "normal.service"), []byte("[Service]\nExecStart=/bin/true\n"), 0644)

	if !isMasked("masked") {
		t.Error("masked.service should be masked")
	}
	if isMasked("normal") {
		t.Error("normal.service should not be masked")
	}
	if isMasked("nonexistent") {
		t.Error("nonexistent should not be masked")
	}
}

func TestSvcEnableDisable(t *testing.T) {
	dir := t.TempDir()
	origDirs := serviceDirs
	serviceDirs = []string{dir}
	defer func() { serviceDirs = origDirs }()

	svcPath := filepath.Join(dir, "test.service")
	os.WriteFile(svcPath, []byte("[Service]\nExecStart=/bin/true\n[Install]\nWantedBy=multi-user.target\n"), 0644)

	// Create the wants directory where enable will create symlinks.
	wantsDir := "/etc/systemd/system/multi-user.target.wants"
	os.MkdirAll(wantsDir, 0755)
	defer os.RemoveAll("/etc/systemd/system/multi-user.target.wants")

	if svcIsEnabled("test") {
		t.Error("should not be enabled before enable")
	}

	if err := svcEnable("test"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	if !svcIsEnabled("test") {
		t.Error("should be enabled after enable")
	}

	if err := svcDisable("test"); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if svcIsEnabled("test") {
		t.Error("should not be enabled after disable")
	}
}

func TestSvcShowProperties(t *testing.T) {
	dir := t.TempDir()
	origDirs := serviceDirs
	serviceDirs = []string{dir}
	defer func() { serviceDirs = origDirs }()

	svcPath := filepath.Join(dir, "test.service")
	os.WriteFile(svcPath, []byte(`[Service]
ExecStart=/usr/bin/test
ExecReload=/bin/kill -HUP $MAINPID
`), 0644)

	// Capture svcShow output.
	tests := []struct {
		prop, valueOnly, wantContains string
	}{
		{"LoadState", "false", "LoadState=loaded"},
		{"CanReload", "false", "CanReload=yes"},
		{"SourcePath", "false", "SourcePath=" + svcPath},
	}

	for _, tt := range tests {
		old := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		svcShow("test", tt.prop, tt.valueOnly == "true")

		w.Close()
		os.Stdout = old
		buf := make([]byte, 1024)
		n, _ := r.Read(buf)
		got := strings.TrimSpace(string(buf[:n]))

		if !strings.Contains(got, tt.wantContains) {
			t.Errorf("show -p %s: got %q, want contains %q", tt.prop, got, tt.wantContains)
		}
	}

	// Test with a service that has no ExecReload.
	svcPath2 := filepath.Join(dir, "noreload.service")
	os.WriteFile(svcPath2, []byte("[Service]\nExecStart=/usr/bin/test\n"), 0644)

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	svcShow("noreload", "CanReload", false)
	w.Close()
	os.Stdout = old
	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	got := strings.TrimSpace(string(buf[:n]))
	if !strings.Contains(got, "CanReload=no") {
		t.Errorf("noreload CanReload: got %q, want CanReload=no", got)
	}
}

func TestSvcShowValueOnly(t *testing.T) {
	dir := t.TempDir()
	origDirs := serviceDirs
	serviceDirs = []string{dir}
	defer func() { serviceDirs = origDirs }()

	svcPath := filepath.Join(dir, "test.service")
	os.WriteFile(svcPath, []byte("[Service]\nExecStart=/usr/bin/test\n"), 0644)

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	svcShow("test", "SourcePath", true)
	w.Close()
	os.Stdout = old
	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	got := strings.TrimSpace(string(buf[:n]))

	// --value should print JUST the path, not SourcePath=<path>
	if strings.Contains(got, "=") {
		t.Errorf("--value mode should not contain '=', got %q", got)
	}
	if got != svcPath {
		t.Errorf("--value SourcePath = %q, want %q", got, svcPath)
	}
}

func TestSvcShowNotFound(t *testing.T) {
	dir := t.TempDir()
	origDirs := serviceDirs
	serviceDirs = []string{dir}
	defer func() { serviceDirs = origDirs }()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	svcShow("nonexistent", "LoadState", false)
	w.Close()
	os.Stdout = old
	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	got := strings.TrimSpace(string(buf[:n]))

	if !strings.Contains(got, "not-found") {
		t.Errorf("nonexistent LoadState: got %q, want contains 'not-found'", got)
	}
}

func TestBuildServiceEnv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "defaults")
	os.WriteFile(envFile, []byte("KEY1=val1\n# comment\nKEY2=val2\n"), 0644)

	svc := serviceFile{sections: map[string][]kvPair{
		"Service": {
			{key: "Environment", value: `"FOO=bar"`},
			{key: "Environment", value: `"BAZ=qux"`},
			{key: "EnvironmentFile", value: envFile},
		},
	}}

	env := buildServiceEnv(svc)

	has := func(needle string) bool {
		for _, e := range env {
			if e == needle {
				return true
			}
		}
		return false
	}

	if !has("FOO=bar") {
		t.Error("missing FOO=bar from Environment=")
	}
	if !has("BAZ=qux") {
		t.Error("missing BAZ=qux from Environment=")
	}
	if !has("KEY1=val1") {
		t.Error("missing KEY1=val1 from EnvironmentFile")
	}
	if !has("KEY2=val2") {
		t.Error("missing KEY2=val2 from EnvironmentFile")
	}
}

func TestBuildServiceEnvMissingFile(t *testing.T) {
	svc := serviceFile{sections: map[string][]kvPair{
		"Service": {
			{key: "EnvironmentFile", value: "-/nonexistent/path"},
		},
	}}

	// Should not panic or error — leading '-' means ignore if missing.
	env := buildServiceEnv(svc)
	if len(env) == 0 {
		t.Error("expected at least inherited env vars")
	}
}

func TestMaskUnmask(t *testing.T) {
	dir := t.TempDir()
	origDirs := serviceDirs
	serviceDirs = []string{dir}
	defer func() { serviceDirs = origDirs }()

	// Create a normal service file first.
	svcPath := filepath.Join(dir, "test.service")
	os.WriteFile(svcPath, []byte("[Service]\nExecStart=/bin/true\n"), 0644)

	// Mask it — creates /etc/systemd/system/test.service -> /dev/null
	etcDir := "/etc/systemd/system"
	os.MkdirAll(etcDir, 0755)
	defer os.Remove(filepath.Join(etcDir, "test.service"))

	if err := svcMask("test"); err != nil {
		t.Fatalf("mask: %v", err)
	}
	if !isMasked("test") {
		t.Error("should be masked after mask")
	}

	if err := svcUnmask("test"); err != nil {
		t.Fatalf("unmask: %v", err)
	}
	if isMasked("test") {
		t.Error("should not be masked after unmask")
	}
}
