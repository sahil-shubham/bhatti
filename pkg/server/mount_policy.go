package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// canonicalHostDirectory never resolves a relative path against the daemon's
// working directory: a request or config typo must not gain ambient access.
func canonicalHostDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q must be absolute", path)
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", path, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", path)
	}
	return canonical, nil
}

func pathWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func pathsOverlap(a, b string) bool {
	return pathWithin(a, b) || pathWithin(b, a)
}

func (s *Server) mountConfigFile() string {
	if s.configPath != "" {
		return s.configPath
	}
	return "server config file (BHATTI_CONFIG, e.g. ~/.bhatti/local.yaml, or /etc/bhatti/config.yaml)"
}

// authorizeMounts resolves each source once and replaces the wire paths with
// canonical paths before the engine/VMM consume them. Both roots and sources
// must be disjoint from server state: accepting an ancestor would expose all
// tenants' disks, while accepting a descendant would expose the state itself.
func (s *Server) authorizeMounts(mounts []engine.FsMount) (int, string) {
	if len(mounts) == 0 {
		return 0, ""
	}
	config := s.mountConfigFile()
	if len(s.mountRoots) == 0 {
		return http.StatusForbidden, fmt.Sprintf("live mounts disabled: set mount_roots in %s", config)
	}

	dataDir, err := canonicalHostDirectory(s.dataDir)
	if err != nil || dataDir == string(filepath.Separator) {
		return http.StatusForbidden, fmt.Sprintf("invalid data_dir in %s: %v", config, err)
	}
	protected := []string{dataDir} // age.key lives under data_dir
	configPaths := s.configPaths
	if len(configPaths) == 0 && s.configPath != "" {
		configPaths = []string{s.configPath}
	}
	for _, path := range configPaths {
		if !filepath.IsAbs(path) {
			return http.StatusForbidden, fmt.Sprintf("invalid config file path %q: must be absolute", path)
		}
		configDir, err := canonicalHostDirectory(filepath.Dir(path))
		if err != nil {
			return http.StatusForbidden, fmt.Sprintf("invalid config directory in %s: %v", path, err)
		}
		protected = append(protected, configDir)
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return http.StatusForbidden, fmt.Sprintf("invalid config file %s: %v", path, err)
		}
		resolvedDir, err := canonicalHostDirectory(filepath.Dir(resolved))
		if err != nil {
			return http.StatusForbidden, fmt.Sprintf("invalid config directory in %s: %v", path, err)
		}
		protected = append(protected, resolvedDir)
	}

	roots := make([]string, 0, len(s.mountRoots))
	for _, raw := range s.mountRoots {
		root, err := canonicalHostDirectory(raw)
		if err != nil {
			return http.StatusForbidden, fmt.Sprintf("invalid mount_roots entry %q in %s: %v", raw, config, err)
		}
		if root == string(filepath.Separator) {
			return http.StatusForbidden, fmt.Sprintf("mount_roots in %s cannot include /", config)
		}
		for _, secret := range protected {
			if pathsOverlap(root, secret) {
				return http.StatusForbidden, fmt.Sprintf("mount_roots in %s cannot include or overlap bhatti's data/config directory", config)
			}
		}
		roots = append(roots, root)
	}

	for i := range mounts {
		canonical, err := canonicalHostDirectory(mounts[i].HostPath)
		if err != nil {
			return http.StatusBadRequest, fmt.Sprintf("mount %d host_path: %v", i, err)
		}
		for _, secret := range protected {
			if pathsOverlap(canonical, secret) {
				return http.StatusForbidden, fmt.Sprintf("mount %d host_path overlaps bhatti's data/config directory", i)
			}
		}
		allowed := false
		for _, root := range roots {
			if pathWithin(canonical, root) {
				allowed = true
				break
			}
		}
		if !allowed {
			return http.StatusForbidden, fmt.Sprintf("mount %d host_path is outside mount_roots in %s", i, config)
		}
		mounts[i].HostPath = canonical
	}
	return 0, ""
}

// checkStoredMounts rechecks the engine's durable bind paths, not client-supplied
// metadata. The engine returns a copy because authorizeMounts canonicalizes it.
func (s *Server) checkStoredMounts(engineID string) error {
	source, ok := s.engine.(interface {
		Mounts(id string) ([]engine.FsMount, bool)
	})
	if !ok {
		return nil // engines without live host mounts have nothing to reauthorize
	}
	mounts, known := source.Mounts(engineID)
	if !known {
		return fmt.Errorf("mount policy: stored mounts unavailable for sandbox %s", engineID)
	}
	if status, reason := s.authorizeMounts(mounts); status != 0 {
		return fmt.Errorf("mount policy violation: %s", reason)
	}
	return nil
}
