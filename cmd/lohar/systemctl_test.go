//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testRegistry constructs a Registry whose Config has ServiceDirs set to
// dir and the rest of the paths defaulted to fresh tempdirs. Tests that
// need different setups (multiple service dirs, specific EtcSystemdDir,
// drop-in dirs) construct NewRegistry(Config{...}) inline.
//
// All the test-time path overrides used to be package-level globals that
// tests rewrote in setup and restored in cleanup. Moving them onto
// Registry.Config makes the immutability that was implied by convention
// into a structural guarantee, and removes the -race finding around
// watcher goroutines reading paths concurrent with cleanup writes.
func testRegistry(t *testing.T, dir string) *Registry {
	t.Helper()
	return NewRegistry(Config{
		ServiceDirs: []string{dir},
		PidDir:      t.TempDir(),
		LogDir:      t.TempDir(),
	})
}

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

	pres := svc.getAll("Service", "ExecStartPre")
	if len(pres) != 1 || pres[0] != "-/usr/bin/test-pre" {
		t.Errorf("getAll ExecStartPre = %v, want [-/usr/bin/test-pre]", pres)
	}
	envs := svc.getAll("Service", "Environment")
	if len(envs) != 1 || envs[0] != `"FOO=bar"` {
		t.Errorf("getAll Environment = %v", envs)
	}
	if got := svc.get("Service", "NoSuchKey"); got != "" {
		t.Errorf("missing key returned %q", got)
	}
}

func TestParseServiceFileBackslashContinuation(t *testing.T) {
	// Real systemd glues a line ending in `\` with the next line. Lots of
	// upstream unit files use this for long ExecStart / Environment values.
	// Before C9 the shim parsed each line independently, so a continued
	// directive was silently truncated at the `\` and the rest of the
	// lines were dropped as orphan key-less lines.
	dir := t.TempDir()
	path := filepath.Join(dir, "cont.service")
	os.WriteFile(path, []byte(`[Service]
Type=simple
ExecStart=/usr/bin/long-daemon \
    --first-flag value-1 \
    --second-flag value-2 \
    --third-flag value-3
Environment=A=1 \
    B=2
Restart=on-failure
`), 0644)

	svc := parseServiceFile(path)

	// The parser glues continued lines with a single space and preserves
	// any leading whitespace from the next line. Multi-space runs are
	// harmless: shell argv-splitting collapses them anyway, so
	// ExecStart="foo     --bar" is equivalent to ExecStart="foo --bar".
	gotStart := svc.get("Service", "ExecStart")
	wantStart := "/usr/bin/long-daemon      --first-flag value-1      --second-flag value-2      --third-flag value-3"
	if gotStart != wantStart {
		t.Errorf("ExecStart joined wrong:\n got: %q\nwant: %q", gotStart, wantStart)
	}

	gotEnv := svc.get("Service", "Environment")
	wantEnv := "A=1      B=2"
	if gotEnv != wantEnv {
		t.Errorf("Environment joined wrong:\n got: %q\nwant: %q", gotEnv, wantEnv)
	}

	if svc.get("Service", "Restart") != "on-failure" {
		t.Errorf("Restart after continuation block lost: %q", svc.get("Service", "Restart"))
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
	svcPath := filepath.Join(dir, "test.service")
	os.WriteFile(svcPath, []byte("[Service]\nExecStart=/bin/true\n"), 0644)

	reg := testRegistry(t, dir)
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

// TestRegistryInvalidateNotFound covers the probe-then-write-then-start
// pattern: an installer calls is-active on a unit that doesn't exist yet
// (caches the miss), writes the unit file, runs daemon-reload, then
// starts the unit. Without InvalidateNotFound() the start fails with
// "Unit not found" because Resolve hits the stale negative cache.
//
// The k3s install script is the canonical offender; this bug blocked
// the G1.3 kubelet-pause-resume spike.
func TestRegistryInvalidateNotFound(t *testing.T) {
	dir := t.TempDir()
	reg := testRegistry(t, dir)

	// First Resolve before the file exists — caches the miss.
	if _, err := reg.Resolve("k3s"); err == nil {
		t.Fatal("Resolve(k3s) on empty dir should fail")
	}

	// Write the unit file. Without InvalidateNotFound, Resolve still
	// returns the cached miss.
	svcPath := filepath.Join(dir, "k3s.service")
	if err := os.WriteFile(svcPath, []byte("[Service]\nExecStart=/usr/local/bin/k3s server\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Resolve("k3s"); err == nil {
		t.Fatal("Resolve(k3s) after write should still fail — negative cache is sticky by design")
	}

	// daemon-reload's job: invalidate the negative cache.
	reg.InvalidateNotFound()

	// Now Resolve picks up the on-disk file.
	u, err := reg.Resolve("k3s")
	if err != nil {
		t.Fatalf("Resolve(k3s) after InvalidateNotFound should succeed: %v", err)
	}
	if u.Canonical != "k3s" || u.Path != svcPath {
		t.Errorf("resolved to wrong unit: %+v", u)
	}
}

func TestRegistryServiceAndSocketAreDistinct(t *testing.T) {
	// Regression for an integration-test failure: openssh-server's
	// postinst calls 'systemctl enable ssh.socket' (resolving .socket)
	// then 'systemctl restart ssh.service' (resolving .service). A bug
	// in the byKey indexing made both names map to the same key ("ssh")
	// regardless of suffix, so the second call returned the .socket
	// Unit and svcStart failed with 'no ExecStart in ssh.socket'.
	//
	// Fix: byKey is now keyed on full name (base+suffix). The two units
	// occupy distinct entries.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ssh.service"),
		[]byte("[Service]\nExecStart=/usr/sbin/sshd -D\n"), 0644)
	os.WriteFile(filepath.Join(dir, "ssh.socket"),
		[]byte("[Socket]\nListenStream=22\nAccept=no\n"), 0644)

	reg := testRegistry(t, dir)

	// Resolve the socket FIRST (mirroring the postinst's order).
	sock, err := reg.Resolve("ssh.socket")
	if err != nil {
		t.Fatalf("Resolve(ssh.socket): %v", err)
	}
	if sock.Suffix != ".socket" {
		t.Errorf("sock.Suffix = %q, want .socket", sock.Suffix)
	}

	// Now resolve the service. Pre-fix, this returned the socket Unit.
	svc, err := reg.Resolve("ssh.service")
	if err != nil {
		t.Fatalf("Resolve(ssh.service): %v", err)
	}
	if svc.Suffix != ".service" {
		t.Errorf("svc.Suffix = %q, want .service (the socket Unit was returned for a service query)", svc.Suffix)
	}
	if svc == sock {
		t.Error("svc and sock returned identical *Unit pointers; .service and .socket must be distinct")
	}
	if !strings.HasSuffix(svc.Path, "ssh.service") {
		t.Errorf("svc.Path = %q, want it to end in ssh.service", svc.Path)
	}

	// And the bare-name resolution still works (defaults to .service).
	bare, err := reg.Resolve("ssh")
	if err != nil {
		t.Fatalf("Resolve(ssh): %v", err)
	}
	if bare != svc {
		t.Errorf("Resolve(ssh) should match Resolve(ssh.service), got different pointers")
	}
}

func TestRegistryAliasResolution(t *testing.T) {
	// The Fastidious bug regression test: ssh.service with Alias=sshd.service.
	// Resolve("ssh") and Resolve("sshd") must return the SAME Unit pointer,
	// so that pidfile/logfile state is shared between the two names.
	dir := t.TempDir()
	svcPath := filepath.Join(dir, "ssh.service")
	os.WriteFile(svcPath, []byte("[Service]\nExecStart=/usr/sbin/sshd\n[Install]\nAlias=sshd.service\n"), 0644)

	reg := testRegistry(t, dir)
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
	wantPid := filepath.Join(reg.Config.PidDir, "ssh.pid")
	if canon.PidPath() != wantPid || alias.PidPath() != wantPid {
		t.Errorf("PidPath canon=%q alias=%q, both want %q",
			canon.PidPath(), alias.PidPath(), wantPid)
	}
}

func TestRegistryAliasInReverse(t *testing.T) {
	// Resolve by alias FIRST, then by canonical -- must still produce one Unit.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ssh.service"),
		[]byte("[Service]\nExecStart=/usr/sbin/sshd\n[Install]\nAlias=sshd.service\n"), 0644)

	reg := testRegistry(t, dir)
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
	realPath := filepath.Join(dir, "foo.service")
	os.WriteFile(realPath, []byte("[Service]\nExecStart=/bin/true\n"), 0644)
	linkPath := filepath.Join(dir, "bar.service")
	os.Symlink(realPath, linkPath)

	reg := testRegistry(t, dir)
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
	os.Symlink("/dev/null", filepath.Join(dir, "masked.service"))

	reg := testRegistry(t, dir)
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
	os.WriteFile(filepath.Join(dir, "postgresql@.service"),
		[]byte("[Service]\nExecStart=/usr/bin/postgres --cluster %i\nWorkingDirectory=/var/lib/%I\n"), 0644)

	reg := testRegistry(t, dir)
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

// dropInTestSetup returns a service-dir and a Registry whose Config has
// the drop-in path pointing at a tempdir. Caller writes its drop-in
// .conf files under that tempdir and resolves through the returned reg.
func dropInTestSetup(t *testing.T) (svcDir, dropInDir string, reg *Registry) {
	t.Helper()
	svcDir = t.TempDir()
	dropInDir = t.TempDir()
	reg = NewRegistry(Config{
		ServiceDirs: []string{svcDir},
		// Highest-priority dir is last in DropInDirs (matches systemd order).
		DropInDirs: []string{dropInDir},
		PidDir:     t.TempDir(),
		LogDir:     t.TempDir(),
	})
	return svcDir, dropInDir, reg
}

func TestDropInScalarOverride(t *testing.T) {
	// Fragment says ExecStart=/bin/old. Drop-in says ExecStart=/bin/new.
	// After resolution, get("Service", "ExecStart") returns the drop-in's
	// value because get() returns the LAST assignment (matching systemd).
	svcDir, dropDir, reg := dropInTestSetup(t)
	os.WriteFile(filepath.Join(svcDir, "foo.service"),
		[]byte("[Service]\nExecStart=/bin/old\n"), 0644)
	os.MkdirAll(filepath.Join(dropDir, "foo.service.d"), 0755)
	os.WriteFile(filepath.Join(dropDir, "foo.service.d", "override.conf"),
		[]byte("[Service]\nExecStart=/bin/new\n"), 0644)

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
	svcDir, dropDir, reg := dropInTestSetup(t)
	os.WriteFile(filepath.Join(svcDir, "foo.service"),
		[]byte("[Service]\nExecStartPre=/bin/a\nExecStart=/bin/main\n"), 0644)
	os.MkdirAll(filepath.Join(dropDir, "foo.service.d"), 0755)
	os.WriteFile(filepath.Join(dropDir, "foo.service.d", "extra.conf"),
		[]byte("[Service]\nExecStartPre=/bin/b\n"), 0644)

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
	svcDir, dropDir, reg := dropInTestSetup(t)
	os.WriteFile(filepath.Join(svcDir, "foo.service"),
		[]byte("[Service]\nExecStartPre=/bin/a\nExecStartPre=/bin/b\nExecStart=/bin/main\n"), 0644)
	os.MkdirAll(filepath.Join(dropDir, "foo.service.d"), 0755)
	os.WriteFile(filepath.Join(dropDir, "foo.service.d", "reset.conf"),
		[]byte("[Service]\nExecStartPre=\nExecStartPre=/bin/c\n"), 0644)

	u, _ := reg.Resolve("foo")
	pres := u.Sections.getAll("Service", "ExecStartPre")
	if len(pres) != 1 || pres[0] != "/bin/c" {
		t.Errorf("ExecStartPre = %v, want [/bin/c]", pres)
	}
}

func TestDropInAlphaOrder(t *testing.T) {
	// Multiple drop-ins in the same dir load alphabetically. The lexically
	// later file wins for scalar directives.
	svcDir, dropDir, reg := dropInTestSetup(t)
	os.WriteFile(filepath.Join(svcDir, "foo.service"),
		[]byte("[Unit]\nDescription=fragment\n[Service]\nExecStart=/bin/x\n"), 0644)
	os.MkdirAll(filepath.Join(dropDir, "foo.service.d"), 0755)
	os.WriteFile(filepath.Join(dropDir, "foo.service.d", "00-first.conf"),
		[]byte("[Unit]\nDescription=zero\n"), 0644)
	os.WriteFile(filepath.Join(dropDir, "foo.service.d", "99-last.conf"),
		[]byte("[Unit]\nDescription=ninetynine\n"), 0644)

	u, _ := reg.Resolve("foo")
	if got := u.Sections.get("Unit", "Description"); got != "ninetynine" {
		t.Errorf("Description = %q, want ninetynine (lexically last drop-in)", got)
	}
}

func TestDropInForAlias(t *testing.T) {
	// ssh.service has Alias=sshd.service. A drop-in placed under the
	// alias name (sshd.service.d/*.conf) is loaded into the resolved
	// Unit -- admins use either name interchangeably.
	svcDir, dropDir, reg := dropInTestSetup(t)
	os.WriteFile(filepath.Join(svcDir, "ssh.service"),
		[]byte("[Service]\nExecStart=/usr/sbin/sshd\n[Install]\nAlias=sshd.service\n"), 0644)
	os.MkdirAll(filepath.Join(dropDir, "sshd.service.d"), 0755)
	os.WriteFile(filepath.Join(dropDir, "sshd.service.d", "port.conf"),
		[]byte("[Service]\nExecStart=\nExecStart=/usr/sbin/sshd -p 2222\n"), 0644)

	u, err := reg.Resolve("ssh")
	if err != nil {
		t.Fatalf("Resolve(ssh): %v", err)
	}
	if got := u.Sections.get("Service", "ExecStart"); got != "/usr/sbin/sshd -p 2222" {
		t.Errorf("ExecStart = %q, want sshd with -p 2222 (alias drop-in applied)", got)
	}
}

func TestDropInPriorityAcrossDirs(t *testing.T) {
	// systemd's drop-in dirs have a precedence order: /etc/ wins over
	// /run/ wins over /usr/lib/. The same .conf basename in multiple
	// dirs gets all loaded (in lowest-priority-first order, so
	// high-priority loads last and overrides). This is what users rely
	// on for distro-overrides workflows: drop a file under /etc/ to
	// override the package-shipped one in /usr/lib/.
	svcDir := t.TempDir()
	lowDir := t.TempDir()  // simulates /usr/lib
	highDir := t.TempDir() // simulates /etc

	reg := NewRegistry(Config{
		ServiceDirs: []string{svcDir},
		// Lowest-priority FIRST so the high-priority one loads last.
		DropInDirs: []string{lowDir, highDir},
		PidDir:     t.TempDir(),
		LogDir:     t.TempDir(),
	})

	os.WriteFile(filepath.Join(svcDir, "foo.service"),
		[]byte("[Service]\nExecStart=/bin/fragment\n"), 0644)

	// Same basename in both dirs. The /etc-equivalent should win.
	os.MkdirAll(filepath.Join(lowDir, "foo.service.d"), 0755)
	os.WriteFile(filepath.Join(lowDir, "foo.service.d", "override.conf"),
		[]byte("[Service]\nExecStart=\nExecStart=/bin/from-usr-lib\n"), 0644)
	os.MkdirAll(filepath.Join(highDir, "foo.service.d"), 0755)
	os.WriteFile(filepath.Join(highDir, "foo.service.d", "override.conf"),
		[]byte("[Service]\nExecStart=\nExecStart=/bin/from-etc\n"), 0644)

	u, _ := reg.Resolve("foo")
	if got := u.Sections.get("Service", "ExecStart"); got != "/bin/from-etc" {
		t.Errorf("ExecStart = %q, want /bin/from-etc (high-priority dir should win)", got)
	}
}

func TestDropInLateAlias(t *testing.T) {
	// When an alias is discovered AFTER the canonical Unit was built
	// (e.g. via inode dedup or alias-scan), drop-ins under that alias's
	// name should still be applied to the existing Unit. Tests the
	// loadDropIns call inside the late-alias paths.
	svcDir, dropDir, reg := dropInTestSetup(t)
	realPath := filepath.Join(svcDir, "foo.service")
	os.WriteFile(realPath, []byte("[Service]\nExecStart=/bin/x\nEnvironment=A=1\n"), 0644)
	os.Symlink(realPath, filepath.Join(svcDir, "bar.service"))

	os.MkdirAll(filepath.Join(dropDir, "bar.service.d"), 0755)
	os.WriteFile(filepath.Join(dropDir, "bar.service.d", "add.conf"),
		[]byte("[Service]\nEnvironment=B=2\n"), 0644)

	_, _ = reg.Resolve("foo")  // canonical first -- no bar drop-in loaded yet
	u, _ := reg.Resolve("bar") // alias -- inode dedup; should also pick up bar.d/

	envs := u.Sections.getAll("Service", "Environment")
	if len(envs) != 2 || envs[0] != "A=1" || envs[1] != "B=2" {
		t.Errorf("Environment = %v, want [A=1 B=2] (late alias drop-in not loaded)", envs)
	}
}

func TestEnableCreatesAliasSymlink(t *testing.T) {
	// C3: when ssh.service has Alias=sshd.service, `systemctl enable ssh`
	// must create /etc/systemd/system/sshd.service as a symlink to the
	// fragment. Real systemd does this so the alias becomes a real
	// filesystem entity that other tools can find by globbing.
	dir := t.TempDir()
	etcDir := t.TempDir()
	reg := NewRegistry(Config{
		ServiceDirs:   []string{dir, etcDir},
		EtcSystemdDir: etcDir,
		PidDir:        t.TempDir(),
		LogDir:        t.TempDir(),
	})

	svcPath := filepath.Join(dir, "ssh.service")
	os.WriteFile(svcPath,
		[]byte("[Service]\nExecStart=/usr/sbin/sshd\n[Install]\nWantedBy=multi-user.target\nAlias=sshd.service\n"),
		0644)

	u, err := reg.Resolve("ssh")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := svcEnable(u); err != nil {
		t.Fatalf("enable: %v", err)
	}

	aliasLink := filepath.Join(etcDir, "sshd.service")
	target, err := os.Readlink(aliasLink)
	if err != nil {
		t.Fatalf("alias symlink %s missing: %v", aliasLink, err)
	}
	if target != svcPath {
		t.Errorf("alias symlink target = %q, want %q", target, svcPath)
	}

	// Disable should remove both the wants/ symlink AND the alias symlink.
	if err := svcDisable(u); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := os.Lstat(aliasLink); !os.IsNotExist(err) {
		t.Errorf("alias symlink still present after disable: %v", err)
	}
	wantsLink := filepath.Join(etcDir, "multi-user.target.wants", "ssh.service")
	if _, err := os.Lstat(wantsLink); !os.IsNotExist(err) {
		t.Errorf("wants symlink still present after disable: %v", err)
	}
}

func TestEnableAliasMakesColdResolutionWork(t *testing.T) {
	// After enable creates the alias symlink, a fresh Registry (cold load)
	// should resolve the alias name through the symlink-by-inode path --
	// without re-parsing the [Install] Alias= directive. This is what
	// makes the alias visible across reboots and to anything that just
	// scans the directory.
	dir := t.TempDir()
	etcDir := t.TempDir()
	cfg := Config{
		// etcDir searched FIRST so its symlinks are preferred.
		ServiceDirs:   []string{etcDir, dir},
		EtcSystemdDir: etcDir,
		PidDir:        t.TempDir(),
		LogDir:        t.TempDir(),
	}

	svcPath := filepath.Join(dir, "ssh.service")
	os.WriteFile(svcPath,
		[]byte("[Service]\nExecStart=/usr/sbin/sshd\n[Install]\nAlias=sshd.service\n"),
		0644)

	reg1 := NewRegistry(cfg)
	u, _ := reg1.Resolve("ssh")
	if err := svcEnable(u); err != nil {
		t.Fatalf("enable: %v", err)
	}

	// Strip the [Install] Alias= directive from the fragment to prove
	// cold resolution is using the symlink-by-inode path, not re-parsing
	// the directive. (After enable on real systemd, you can edit the
	// fragment to remove Alias= and the alias still works because the
	// symlink is now the source of truth.)
	os.WriteFile(svcPath, []byte("[Service]\nExecStart=/usr/sbin/sshd\n"), 0644)

	// Fresh registry against the same Config: no memoisation from reg1.
	reg2 := NewRegistry(cfg)
	canon, err := reg2.Resolve("ssh")
	if err != nil {
		t.Fatalf("Resolve(ssh): %v", err)
	}
	alias, err := reg2.Resolve("sshd")
	if err != nil {
		t.Fatalf("Resolve(sshd): %v", err)
	}
	if canon != alias {
		t.Fatalf("cold resolution should unify via symlink inode, got %p vs %p", canon, alias)
	}
}

func TestUnitStateUnification(t *testing.T) {
	// The whole point of C1: pidfile + logfile paths are keyed by
	// canonical name, so any alias query observes the same state.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ssh.service"),
		[]byte("[Service]\nExecStart=/usr/sbin/sshd\n[Install]\nAlias=sshd.service\n"), 0644)

	reg := testRegistry(t, dir)
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
	reg := NewRegistry(Config{
		ServiceDirs:   []string{dir, etcDir},
		EtcSystemdDir: etcDir,
		PidDir:        t.TempDir(),
		LogDir:        t.TempDir(),
	})

	svcPath := filepath.Join(dir, "test.service")
	os.WriteFile(svcPath, []byte("[Service]\nExecStart=/bin/true\n[Install]\nWantedBy=multi-user.target\n"), 0644)

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
	svcPath := filepath.Join(dir, "test.service")
	os.WriteFile(svcPath, []byte(`[Service]
ExecStart=/usr/bin/test
ExecReload=/bin/kill -HUP $MAINPID
`), 0644)

	tests := []struct {
		prop, valueOnly, wantContains string
	}{
		{"LoadState", "false", "LoadState=loaded"},
		{"CanReload", "false", "CanReload=yes"},
		{"SourcePath", "false", "SourcePath=" + svcPath},
	}

	reg := testRegistry(t, dir)
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
	ureg := testRegistry(t, dir)
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
	svcPath := filepath.Join(dir, "test.service")
	os.WriteFile(svcPath, []byte("[Service]\nExecStart=/usr/bin/test\n"), 0644)

	reg := testRegistry(t, dir)
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

	// --value should print JUST the path, not SourcePath=<path>.
	if strings.Contains(got, "=") {
		t.Errorf("--value mode should not contain '=', got %q", got)
	}
	if got != svcPath {
		t.Errorf("--value SourcePath = %q, want %q", got, svcPath)
	}
}

func TestSvcShowNotFound(t *testing.T) {
	dir := t.TempDir()
	reg := testRegistry(t, dir)
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

	// Should not panic or error -- leading '-' means ignore if missing.
	env := buildServiceEnv(svc)
	if len(env) == 0 {
		t.Error("expected at least inherited env vars")
	}
}

func TestMaskUnmask(t *testing.T) {
	dir := t.TempDir()
	etcDir := t.TempDir()
	cfg := Config{
		// etcDir searched FIRST so the mask symlink overrides the real file.
		ServiceDirs:   []string{etcDir, dir},
		EtcSystemdDir: etcDir,
		PidDir:        t.TempDir(),
		LogDir:        t.TempDir(),
	}
	reg := NewRegistry(cfg)

	// Create a normal service file first.
	svcPath := filepath.Join(dir, "test.service")
	os.WriteFile(svcPath, []byte("[Service]\nExecStart=/bin/true\n"), 0644)

	if err := reg.svcMaskName("test"); err != nil {
		t.Fatalf("mask: %v", err)
	}
	isMasked := func(r *Registry, name string) bool {
		u, err := r.Resolve(name)
		return err == nil && u.Masked
	}
	if !isMasked(reg, "test") {
		t.Error("should be masked after mask")
	}

	if err := reg.svcUnmaskName("test"); err != nil {
		t.Fatalf("unmask: %v", err)
	}
	// Mask state is cached at resolution time, so a fresh registry
	// (against the same Config) is needed to observe post-unmask state.
	reg2 := NewRegistry(cfg)
	if isMasked(reg2, "test") {
		t.Error("should not be masked after unmask")
	}
}
