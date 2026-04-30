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
	// Targets are handled by isTarget() before the registry; svcIsActive
	// itself only operates on resolved Units (services).
	if !isTarget("sysinit") {
		t.Error("sysinit.target should be a target")
	}
	if !isTarget("multi-user") {
		t.Error("multi-user.target should be a target")
	}
}

func TestRegistryResolveDirect(t *testing.T) {
	dir := t.TempDir()
	origDirs := serviceDirs
	serviceDirs = []string{dir}
	defer func() { serviceDirs = origDirs }()

	svcPath := filepath.Join(dir, "test.service")
	os.WriteFile(svcPath, []byte("[Service]\nExecStart=/bin/true\n"), 0644)

	reg := NewRegistry()
	u, err := reg.Resolve("test")
	if err != nil {
		t.Fatalf("Resolve(test): %v", err)
	}
	if u.Canonical != "test" || u.Suffix != ".service" || u.Path != svcPath {
		t.Errorf("got Unit{%q, %q, %q}, want test/.service/%s", u.Canonical, u.Suffix, u.Path, svcPath)
	}

	if _, err := reg.Resolve("nonexistent"); err == nil {
		t.Error("Resolve(nonexistent) should return error")
	}
}

func TestRegistryAliasResolution(t *testing.T) {
	// The Fastidious bug regression test: ssh.service with Alias=sshd.service.
	// Resolve("ssh") and Resolve("sshd") must return the SAME Unit pointer,
	// so that pidfile/logfile state is shared between the two names.
	dir := t.TempDir()
	origDirs := serviceDirs
	serviceDirs = []string{dir}
	defer func() { serviceDirs = origDirs }()

	svcPath := filepath.Join(dir, "ssh.service")
	os.WriteFile(svcPath, []byte("[Service]\nExecStart=/usr/sbin/sshd\n[Install]\nAlias=sshd.service\n"), 0644)

	reg := NewRegistry()
	canon, err := reg.Resolve("ssh")
	if err != nil {
		t.Fatalf("Resolve(ssh): %v", err)
	}
	alias, err := reg.Resolve("sshd")
	if err != nil {
		t.Fatalf("Resolve(sshd): %v", err)
	}
	if canon != alias {
		t.Fatalf("Resolve(ssh) and Resolve(sshd) returned different *Unit pointers: %p vs %p", canon, alias)
	}
	if canon.Canonical != "ssh" {
		t.Errorf("Canonical = %q, want ssh", canon.Canonical)
	}
	if _, ok := canon.Aliases["sshd"]; !ok {
		t.Errorf("Aliases = %v, want sshd present", canon.Aliases)
	}

	// State paths must use the canonical name regardless of which alias
	// was queried. This is the bug fix.
	wantPid := filepath.Join(pidDir, "ssh.pid")
	if canon.PidPath() != wantPid || alias.PidPath() != wantPid {
		t.Errorf("PidPath canon=%q alias=%q, both want %q",
			canon.PidPath(), alias.PidPath(), wantPid)
	}
}

func TestRegistryAliasInReverse(t *testing.T) {
	// Resolve by alias FIRST, then by canonical — must still produce one Unit.
	dir := t.TempDir()
	origDirs := serviceDirs
	serviceDirs = []string{dir}
	defer func() { serviceDirs = origDirs }()

	os.WriteFile(filepath.Join(dir, "ssh.service"),
		[]byte("[Service]\nExecStart=/usr/sbin/sshd\n[Install]\nAlias=sshd.service\n"), 0644)

	reg := NewRegistry()
	alias, err := reg.Resolve("sshd")
	if err != nil {
		t.Fatalf("Resolve(sshd): %v", err)
	}
	canon, err := reg.Resolve("ssh")
	if err != nil {
		t.Fatalf("Resolve(ssh): %v", err)
	}
	if canon != alias {
		t.Fatalf("alias-first resolution should still produce identical pointers, got %p vs %p", alias, canon)
	}
	if canon.Canonical != "ssh" {
		t.Errorf("Canonical = %q, want ssh (the file basename, not the queried alias)", canon.Canonical)
	}
}

func TestRegistrySymlinkAlias(t *testing.T) {
	// Two unit-file paths pointing at the same inode (symlink) resolve
	// to the same Unit through inode dedup.
	dir := t.TempDir()
	origDirs := serviceDirs
	serviceDirs = []string{dir}
	defer func() { serviceDirs = origDirs }()

	realPath := filepath.Join(dir, "foo.service")
	os.WriteFile(realPath, []byte("[Service]\nExecStart=/bin/true\n"), 0644)
	linkPath := filepath.Join(dir, "bar.service")
	os.Symlink(realPath, linkPath)

	reg := NewRegistry()
	foo, err := reg.Resolve("foo")
	if err != nil {
		t.Fatalf("Resolve(foo): %v", err)
	}
	bar, err := reg.Resolve("bar")
	if err != nil {
		t.Fatalf("Resolve(bar): %v", err)
	}
	if foo != bar {
		t.Fatalf("symlinked-alias should resolve to same Unit, got %p vs %p", foo, bar)
	}
}

func TestRegistryMasked(t *testing.T) {
	dir := t.TempDir()
	origDirs := serviceDirs
	serviceDirs = []string{dir}
	defer func() { serviceDirs = origDirs }()

	os.Symlink("/dev/null", filepath.Join(dir, "masked.service"))

	reg := NewRegistry()
	u, err := reg.Resolve("masked")
	if err != nil {
		t.Fatalf("Resolve(masked) should succeed with Masked=true, got err=%v", err)
	}
	if !u.Masked {
		t.Errorf("Resolve(masked).Masked = false, want true")
	}
}

func TestRegistryTemplateInstance(t *testing.T) {
	// postgresql@16-main resolves against postgresql@.service, with
	// %i / %I expanded in the parsed sections.
	dir := t.TempDir()
	origDirs := serviceDirs
	serviceDirs = []string{dir}
	defer func() { serviceDirs = origDirs }()

	os.WriteFile(filepath.Join(dir, "postgresql@.service"),
		[]byte("[Service]\nExecStart=/usr/bin/postgres --cluster %i\nWorkingDirectory=/var/lib/%I\n"), 0644)

	reg := NewRegistry()
	u, err := reg.Resolve("postgresql@16-main")
	if err != nil {
		t.Fatalf("Resolve(postgresql@16-main): %v", err)
	}
	if u.Instance != "16-main" {
		t.Errorf("Instance = %q, want 16-main", u.Instance)
	}
	if u.Template != "postgresql" {
		t.Errorf("Template = %q, want postgresql", u.Template)
	}
	if got := u.Sections.get("Service", "ExecStart"); got != "/usr/bin/postgres --cluster 16-main" {
		t.Errorf("%%i not expanded: %q", got)
	}
	if got := u.Sections.get("Service", "WorkingDirectory"); got != "/var/lib/16/main" {
		t.Errorf("%%I not expanded (- should become /): %q", got)
	}
}

// dropInTestSetup wires up serviceDirs + dropInDirs against tempdirs and
// returns the path of the drop-in dir that takes highest priority. Caller
// gets a cleanup func via t.Cleanup.
func dropInTestSetup(t *testing.T) (svcDir, etcDropInDir string) {
	t.Helper()
	svcDir = t.TempDir()
	etcDropInDir = t.TempDir()
	origSvc := serviceDirs
	origDrop := dropInDirs
	serviceDirs = []string{svcDir}
	// Highest-priority dir is last in dropInDirs (matches systemd order).
	dropInDirs = []string{etcDropInDir}
	t.Cleanup(func() {
		serviceDirs = origSvc
		dropInDirs = origDrop
	})
	return svcDir, etcDropInDir
}

func TestDropInScalarOverride(t *testing.T) {
	// Fragment says ExecStart=/bin/old. Drop-in says ExecStart=/bin/new.
	// After resolution, get("Service", "ExecStart") returns the drop-in's
	// value because get() returns the LAST assignment (matching systemd).
	svcDir, dropDir := dropInTestSetup(t)
	os.WriteFile(filepath.Join(svcDir, "foo.service"),
		[]byte("[Service]\nExecStart=/bin/old\n"), 0644)
	os.MkdirAll(filepath.Join(dropDir, "foo.service.d"), 0755)
	os.WriteFile(filepath.Join(dropDir, "foo.service.d", "override.conf"),
		[]byte("[Service]\nExecStart=/bin/new\n"), 0644)

	reg := NewRegistry()
	u, err := reg.Resolve("foo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := u.Sections.get("Service", "ExecStart"); got != "/bin/new" {
		t.Errorf("ExecStart = %q, want /bin/new", got)
	}
}

func TestDropInListAccumulate(t *testing.T) {
	// ExecStartPre is a list directive: drop-in entries APPEND to the
	// fragment's entries.
	svcDir, dropDir := dropInTestSetup(t)
	os.WriteFile(filepath.Join(svcDir, "foo.service"),
		[]byte("[Service]\nExecStartPre=/bin/a\nExecStart=/bin/main\n"), 0644)
	os.MkdirAll(filepath.Join(dropDir, "foo.service.d"), 0755)
	os.WriteFile(filepath.Join(dropDir, "foo.service.d", "extra.conf"),
		[]byte("[Service]\nExecStartPre=/bin/b\n"), 0644)

	reg := NewRegistry()
	u, _ := reg.Resolve("foo")
	pres := u.Sections.getAll("Service", "ExecStartPre")
	if len(pres) != 2 || pres[0] != "/bin/a" || pres[1] != "/bin/b" {
		t.Errorf("ExecStartPre = %v, want [/bin/a /bin/b]", pres)
	}
}

func TestDropInResetSemantics(t *testing.T) {
	// systemd's drop-in idiom for replacing a list: empty assignment
	// resets the list, subsequent values establish the new contents.
	//   [Service]
	//   ExecStartPre=
	//   ExecStartPre=/bin/c
	// After reset, only /bin/c remains.
	svcDir, dropDir := dropInTestSetup(t)
	os.WriteFile(filepath.Join(svcDir, "foo.service"),
		[]byte("[Service]\nExecStartPre=/bin/a\nExecStartPre=/bin/b\nExecStart=/bin/main\n"), 0644)
	os.MkdirAll(filepath.Join(dropDir, "foo.service.d"), 0755)
	os.WriteFile(filepath.Join(dropDir, "foo.service.d", "reset.conf"),
		[]byte("[Service]\nExecStartPre=\nExecStartPre=/bin/c\n"), 0644)

	reg := NewRegistry()
	u, _ := reg.Resolve("foo")
	pres := u.Sections.getAll("Service", "ExecStartPre")
	if len(pres) != 1 || pres[0] != "/bin/c" {
		t.Errorf("ExecStartPre = %v, want [/bin/c]", pres)
	}
}

func TestDropInAlphaOrder(t *testing.T) {
	// Multiple drop-ins in the same dir load alphabetically. The lexically
	// later file wins for scalar directives.
	svcDir, dropDir := dropInTestSetup(t)
	os.WriteFile(filepath.Join(svcDir, "foo.service"),
		[]byte("[Unit]\nDescription=fragment\n[Service]\nExecStart=/bin/x\n"), 0644)
	os.MkdirAll(filepath.Join(dropDir, "foo.service.d"), 0755)
	os.WriteFile(filepath.Join(dropDir, "foo.service.d", "00-first.conf"),
		[]byte("[Unit]\nDescription=zero\n"), 0644)
	os.WriteFile(filepath.Join(dropDir, "foo.service.d", "99-last.conf"),
		[]byte("[Unit]\nDescription=ninetynine\n"), 0644)

	reg := NewRegistry()
	u, _ := reg.Resolve("foo")
	if got := u.Sections.get("Unit", "Description"); got != "ninetynine" {
		t.Errorf("Description = %q, want ninetynine (lexically last drop-in)", got)
	}
}

func TestDropInForAlias(t *testing.T) {
	// ssh.service has Alias=sshd.service. A drop-in placed under the
	// alias name (sshd.service.d/*.conf) is loaded into the resolved
	// Unit — admins use either name interchangeably.
	svcDir, dropDir := dropInTestSetup(t)
	os.WriteFile(filepath.Join(svcDir, "ssh.service"),
		[]byte("[Service]\nExecStart=/usr/sbin/sshd\n[Install]\nAlias=sshd.service\n"), 0644)
	os.MkdirAll(filepath.Join(dropDir, "sshd.service.d"), 0755)
	os.WriteFile(filepath.Join(dropDir, "sshd.service.d", "port.conf"),
		[]byte("[Service]\nExecStart=\nExecStart=/usr/sbin/sshd -p 2222\n"), 0644)

	reg := NewRegistry()
	// Resolve by canonical name, but the drop-in lives under the alias.
	u, err := reg.Resolve("ssh")
	if err != nil {
		t.Fatalf("Resolve(ssh): %v", err)
	}
	if got := u.Sections.get("Service", "ExecStart"); got != "/usr/sbin/sshd -p 2222" {
		t.Errorf("ExecStart = %q, want sshd with -p 2222 (alias drop-in applied)", got)
	}
}

func TestDropInLateAlias(t *testing.T) {
	// When an alias is discovered AFTER the canonical Unit was built
	// (e.g. via inode dedup or alias-scan), drop-ins under that alias's
	// name should still be applied to the existing Unit. Tests the
	// loadDropIns call inside the late-alias paths.
	svcDir, dropDir := dropInTestSetup(t)
	realPath := filepath.Join(svcDir, "foo.service")
	os.WriteFile(realPath, []byte("[Service]\nExecStart=/bin/x\nEnvironment=A=1\n"), 0644)
	os.Symlink(realPath, filepath.Join(svcDir, "bar.service"))

	os.MkdirAll(filepath.Join(dropDir, "bar.service.d"), 0755)
	os.WriteFile(filepath.Join(dropDir, "bar.service.d", "add.conf"),
		[]byte("[Service]\nEnvironment=B=2\n"), 0644)

	reg := NewRegistry()
	_, _ = reg.Resolve("foo")  // canonical first — no bar drop-in loaded yet
	u, _ := reg.Resolve("bar") // alias — inode dedup; should also pick up bar.d/

	envs := u.Sections.getAll("Service", "Environment")
	if len(envs) != 2 || envs[0] != "A=1" || envs[1] != "B=2" {
		t.Errorf("Environment = %v, want [A=1 B=2] (late alias drop-in not loaded)", envs)
	}
}

func TestUnitStateUnification(t *testing.T) {
	// The whole point of C1: pidfile + logfile paths are keyed by
	// canonical name, so any alias query observes the same state.
	dir := t.TempDir()
	origDirs := serviceDirs
	serviceDirs = []string{dir}
	defer func() { serviceDirs = origDirs }()

	os.WriteFile(filepath.Join(dir, "ssh.service"),
		[]byte("[Service]\nExecStart=/usr/sbin/sshd\n[Install]\nAlias=sshd.service\n"), 0644)

	reg := NewRegistry()
	canon, _ := reg.Resolve("ssh")
	alias, _ := reg.Resolve("sshd")

	if canon.PidPath() != alias.PidPath() {
		t.Errorf("PidPath: canon=%q alias=%q", canon.PidPath(), alias.PidPath())
	}
	if canon.LogPath() != alias.LogPath() {
		t.Errorf("LogPath: canon=%q alias=%q", canon.LogPath(), alias.LogPath())
	}
}

func TestSvcEnableDisable(t *testing.T) {
	dir := t.TempDir()
	etcDir := t.TempDir()
	origDirs := serviceDirs
	origEtc := etcSystemdDir
	serviceDirs = []string{dir, etcDir}
	etcSystemdDir = etcDir
	defer func() { serviceDirs = origDirs; etcSystemdDir = origEtc }()

	svcPath := filepath.Join(dir, "test.service")
	os.WriteFile(svcPath, []byte("[Service]\nExecStart=/bin/true\n[Install]\nWantedBy=multi-user.target\n"), 0644)

	reg := NewRegistry()
	u, err := reg.Resolve("test")
	if err != nil {
		t.Fatalf("Resolve(test): %v", err)
	}

	if svcIsEnabled(u) {
		t.Error("should not be enabled before enable")
	}

	if err := svcEnable(u); err != nil {
		t.Fatalf("enable: %v", err)
	}

	if !svcIsEnabled(u) {
		t.Error("should be enabled after enable")
	}

	if err := svcDisable(u); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if svcIsEnabled(u) {
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

	reg := NewRegistry()
	u, _ := reg.Resolve("test")

	for _, tt := range tests {
		old := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		svcShow(u, "test", tt.prop, tt.valueOnly == "true")

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
	ureg := NewRegistry()
	unoreload, _ := ureg.Resolve("noreload")

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	svcShow(unoreload, "noreload", "CanReload", false)
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

	reg := NewRegistry()
	u, _ := reg.Resolve("test")

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	svcShow(u, "test", "SourcePath", true)
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

	reg := NewRegistry()
	u, _ := reg.Resolve("nonexistent") // err is non-nil; u is nil

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	svcShow(u, "nonexistent", "LoadState", false)
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
	etcDir := t.TempDir()
	origDirs := serviceDirs
	origEtc := etcSystemdDir
	// etcDir is searched FIRST so the mask symlink overrides the real file.
	serviceDirs = []string{etcDir, dir}
	etcSystemdDir = etcDir
	defer func() { serviceDirs = origDirs; etcSystemdDir = origEtc }()

	// Create a normal service file first.
	svcPath := filepath.Join(dir, "test.service")
	os.WriteFile(svcPath, []byte("[Service]\nExecStart=/bin/true\n"), 0644)

	if err := svcMaskName("test"); err != nil {
		t.Fatalf("mask: %v", err)
	}
	reg := NewRegistry()
	isMasked := func(name string) bool {
		u, err := reg.Resolve(name)
		return err == nil && u.Masked
	}
	if !isMasked("test") {
		t.Error("should be masked after mask")
	}

	if err := svcUnmaskName("test"); err != nil {
		t.Fatalf("unmask: %v", err)
	}
	// Reset registry: mask state is cached at resolution time, so a fresh
	// registry is needed to observe the post-unmask state.
	reg = NewRegistry()
	if isMasked("test") {
		t.Error("should not be masked after unmask")
	}
}
