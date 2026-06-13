package pkg

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

// Config holds all bhatti configuration.
type Config struct {
	Engine    string `yaml:"engine"`     // "firecracker" (default)
	Listen    string `yaml:"listen"`     // e.g. ":8080"
	APIURL    string `yaml:"api_url"`    // CLI: remote API endpoint (e.g. https://api.bhatti.sh)
	AuthToken string `yaml:"auth_token"` // CLI: API key for remote requests
	DataDir   string `yaml:"data_dir"`   // defaults to ~/.bhatti

	// ConfigPath is the file path LoadConfig actually loaded from.
	// Not persisted — set at load time for logging/debugging.
	ConfigPath string `yaml:"-"`

	// Public proxy (Phase 1: path-based, for dev/testing)
	PublicProxyListen string `yaml:"public_proxy_listen,omitempty"` // e.g. ":8443"

	// Domain mode (Phase 2: host-based routing + TLS)
	Domain *DomainConfig `yaml:"domain,omitempty"`

	// Firecracker-specific
	FirecrackerBin    string `yaml:"firecracker_bin"`    // path to firecracker binary
	FirecrackerKernel string `yaml:"firecracker_kernel"` // path to vmlinux
	FirecrackerRootfs string `yaml:"firecracker_rootfs"` // path to base rootfs.ext4

	// Krucible-specific (libkrun engine; macOS + Linux)
	KrucibleVMM       string `yaml:"krucible_vmm"`        // path to the bhatti-vmm helper (default: next to binary / PATH)
	KrucibleRootfs    string `yaml:"krucible_rootfs"`     // base rootfs dir (virtiofs root) with /init.krun=lohar
	KrucibleLibDir    string `yaml:"krucible_libdir"`     // dir with libkrun/libkrunfw (default: autodetect)
	KrucibleSocketDir string `yaml:"krucible_socket_dir"` // short dir for vsock UDS (default: /tmp/bhatti-kr)

	// Jailer (empty = bare mode, no isolation)
	FirecrackerJailer string `yaml:"firecracker_jailer,omitempty"` // path to jailer binary
	JailUID           int    `yaml:"jail_uid,omitempty"`           // uid for jailed FC (e.g. 10000)
	JailGID           int    `yaml:"jail_gid,omitempty"`           // gid for jailed FC (e.g. 10000)

	// DNSUpstreams is the ordered list of upstream resolvers each
	// per-user in-cluster DNS responder forwards non-sandbox queries
	// to (G1.1). Empty → engine default (1.1.1.1, 8.8.8.8). Set this
	// for a homelab that runs its own resolver (e.g. Pi-hole) so
	// sandboxes inherit the host's view of the world.
	DNSUpstreams []string `yaml:"dns_upstreams,omitempty"`

	// Backup to S3-compatible storage
	Backup *BackupConfig `yaml:"backup,omitempty"`
}

// BackupConfig configures volume backup to S3-compatible object storage.
type BackupConfig struct {
	S3Endpoint  string           `yaml:"s3_endpoint"`   // e.g. "https://s3.eu-central-003.backblazeb2.com"
	S3Region    string           `yaml:"s3_region"`     // e.g. "eu-central-003"
	S3Bucket    string           `yaml:"s3_bucket"`
	S3AccessKey string           `yaml:"s3_access_key"`
	S3SecretKey string           `yaml:"s3_secret_key"`
	Schedule    []BackupSchedule `yaml:"schedule,omitempty"`
}

// BackupSchedule defines an automatic backup schedule for a volume.
type BackupSchedule struct {
	Volume    string `yaml:"volume"`    // volume name
	Cron      string `yaml:"cron"`      // cron expression (minute hour day month weekday)
	Retention int    `yaml:"retention"` // keep last N backups
}

// DomainConfig configures domain mode with host-based routing and TLS.
type DomainConfig struct {
	APIHost   string `yaml:"api_host"`   // e.g. "api.bhatti.sh"
	ProxyZone string `yaml:"proxy_zone"` // e.g. "bhatti.sh" — published apps get <alias>.bhatti.sh
	ACMEEmail string `yaml:"acme_email"` // for per-alias autocert (fallback)
	TLSCert   string `yaml:"tls_cert"`   // wildcard cert path (recommended)
	TLSKey    string `yaml:"tls_key"`    // wildcard key path
}

// DefaultDataDir returns ~/.bhatti for the *invoking* user.
//
// When the process is running under sudo, os.UserHomeDir() returns
// /var/root (macOS) or /root (Linux), which is almost never what the
// user wants — their CLI config lives in their real home directory.
// We honor SUDO_USER so that `sudo bhatti setup` writes the same file
// `bhatti list` later reads, and we don't leave token configs scattered
// across /root/.bhatti and ~/.bhatti.
//
// Daemon callers (`bhatti serve` under systemd) are unaffected: SUDO_USER
// is unset there, and the server reads /etc/bhatti/config.yaml which
// supplies an explicit data_dir anyway.
func DefaultDataDir() string {
	return filepath.Join(invokingUserHome(), ".bhatti")
}

// invokingUserHome returns the home directory of the user who started
// this process, looking through sudo if applicable.
func invokingUserHome() string {
	if u := invokingUser(); u != nil {
		return u.HomeDir
	}
	home, _ := os.UserHomeDir()
	return home
}

// invokingUser returns the SUDO_USER's *user.User when running under
// sudo, or nil otherwise. Handy when callers need uid/gid for chown.
func invokingUser() *user.User {
	name := os.Getenv("SUDO_USER")
	if name == "" || name == "root" {
		return nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil
	}
	return u
}

// InvokingUserIDs returns (uid, gid, ok) for the SUDO_USER, parsed as
// integers suitable for os.Chown. Returns ok=false if not under sudo or
// the lookup failed.
func InvokingUserIDs() (int, int, bool) {
	u := invokingUser()
	if u == nil {
		return 0, 0, false
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return uid, gid, true
}

// EnsureUserOwnedPath makes sure `path` (which we just created or wrote
// while running under sudo) ends up owned by the invoking user, not
// root. No-op when not running under sudo.
//
// The classic trap this prevents:
//
//	$ sudo bhatti version    # creates /home/alice/.bhatti/ owned by root
//	$ bhatti setup           # EACCES — alice can't write into root's dir
//
// Use after every os.MkdirAll / os.WriteFile that touches a user-home
// path. Errors are intentionally swallowed: we can't recover from a
// failed chown, and the next interactive command will surface the
// permission issue with a clearer message anyway.
func EnsureUserOwnedPath(paths ...string) {
	uid, gid, ok := InvokingUserIDs()
	if !ok {
		return
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		_ = os.Chown(p, uid, gid)
	}
}

// InvokingUID returns the uid of the user who launched the process.
// Under sudo, this is SUDO_USER's uid; otherwise the current uid.
// Used for per-user cache paths that must be stable across sudo and
// non-sudo invocations of the same command.
func InvokingUID() int {
	if uid, _, ok := InvokingUserIDs(); ok {
		return uid
	}
	return os.Getuid()
}

// LoadConfig reads config from multiple locations and layers them:
//
//  1. $BHATTI_CONFIG (explicit path — used alone if set)
//  2. /etc/bhatti/config.yaml (server config — engine, listen, data_dir)
//  3. ~/.bhatti/config.yaml (client config — api_url, auth_token)
//
// On a server machine, both /etc/bhatti/config.yaml and ~/.bhatti/config.yaml
// may exist. The server config provides engine settings, the user config
// provides client credentials. Fields from the user config only fill in
// values that are empty after loading the server config — they never
// override server settings.
//
// For migration: if /var/lib/bhatti/config.yaml exists and nothing above
// matched, it is loaded as a deprecated fallback with a stderr warning.
//
// Returns sensible defaults if no config file is found.
func LoadConfig() (*Config, error) {
	dir := DefaultDataDir()
	cfg := &Config{
		Engine:  "firecracker",
		Listen:  ":8080",
		DataDir: dir,
	}

	// If an explicit config is set, use it alone (no layering).
	if envPath := os.Getenv("BHATTI_CONFIG"); envPath != "" {
		data, err := os.ReadFile(envPath)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", envPath, err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", envPath, err)
		}
		if cfg.DataDir == "" {
			cfg.DataDir = dir
		}
		cfg.ConfigPath = envPath
		return cfg, nil
	}

	// Layer 1: system config (server settings)
	var loadedFrom string
	for _, path := range []string{
		"/etc/bhatti/config.yaml",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
		loadedFrom = path
		break
	}

	// Migration fallback: old location inside data dir.
	// TODO: remove after a few releases (added v1.6.0).
	if loadedFrom == "" {
		const deprecated = "/var/lib/bhatti/config.yaml"
		if data, err := os.ReadFile(deprecated); err == nil {
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse config %s: %w", deprecated, err)
			}
			loadedFrom = deprecated
			fmt.Fprintf(os.Stderr, "⚠ config loaded from deprecated location %s\n  move to /etc/bhatti/config.yaml\n\n", deprecated)
		}
	}

	// Layer 2: user config (client credentials)
	// Only fills in api_url and auth_token if they're still empty.
	userConfig := filepath.Join(dir, "config.yaml")
	if data, err := os.ReadFile(userConfig); err == nil {
		var userCfg Config
		if err := yaml.Unmarshal(data, &userCfg); err == nil {
			if cfg.APIURL == "" && userCfg.APIURL != "" {
				cfg.APIURL = userCfg.APIURL
			}
			if cfg.AuthToken == "" && userCfg.AuthToken != "" {
				cfg.AuthToken = userCfg.AuthToken
			}
			if loadedFrom == "" {
				loadedFrom = userConfig
			}
		}
	}

	if cfg.DataDir == "" {
		cfg.DataDir = dir
	}
	if loadedFrom != "" {
		cfg.ConfigPath = loadedFrom
	}
	return cfg, nil
}

// EnsureKeypair generates an ed25519 SSH keypair in DataDir if missing.
// Returns the path to the private key.
func EnsureKeypair(dataDir string) (string, error) {
	privPath := filepath.Join(dataDir, "id_ed25519")
	pubPath := privPath + ".pub"

	if _, err := os.Stat(privPath); err == nil {
		return privPath, nil
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate keypair: %w", err)
	}

	// Write private key in PEM format
	privBytes, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return "", fmt.Errorf("marshal private key: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return "", err
	}
	if err := os.WriteFile(privPath, pem.EncodeToMemory(privBytes), 0600); err != nil {
		return "", err
	}

	// Write public key in authorized_keys format
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(pubPath, ssh.MarshalAuthorizedKey(sshPub), 0644); err != nil {
		return "", err
	}

	return privPath, nil
}
