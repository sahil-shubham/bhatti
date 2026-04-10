package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

var aliasRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var reservedAliases = map[string]bool{
	"www": true, "mail": true, "admin": true, "status": true,
	"ns1": true, "ns2": true, "api": true, "app": true,
	"_acme-challenge": true,
}

func validateAlias(alias string) error {
	if !aliasRegex.MatchString(alias) {
		return fmt.Errorf("alias must be lowercase alphanumeric with hyphens, 1-63 chars")
	}
	if reservedAliases[alias] {
		return fmt.Errorf("alias %q is reserved", alias)
	}
	return nil
}

// generateAlias creates a <name>-<random> alias. The random suffix prevents
// guessing (2.1B possibilities) and collisions. Format: dev-k3m9x2.bhatti.sh
func generateAlias(sandboxName string) string {
	base := strings.ToLower(sandboxName)
	base = regexp.MustCompile(`[^a-z0-9-]`).ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "sandbox"
	}

	// 6 chars of a-z0-9 = 2.1 billion possibilities
	b := make([]byte, 4)
	rand.Read(b)
	suffix := hex.EncodeToString(b)[:6]

	alias := base + "-" + suffix
	if len(alias) > 63 {
		alias = base[:63-7] + "-" + suffix // 7 = dash + 6 chars
	}
	return alias
}

func generateUniqueAlias(st *store.Store, sandboxName string) (string, error) {
	// Try up to 4 times (each attempt has a fresh random suffix)
	for i := 0; i < 4; i++ {
		candidate := generateAlias(sandboxName)
		if _, err := st.GetPublishRuleByAlias(candidate); err != nil {
			return candidate, nil // not taken
		}
	}
	return "", fmt.Errorf("failed to generate unique alias after 4 attempts")
}

func (s *Server) handleSandboxPublish(w http.ResponseWriter, r *http.Request, id, sub string) {
	switch r.Method {
	case "POST":
		if sub != "" && sub != "/" {
			errResp(w, 404, "not found")
			return
		}
		s.handlePublish(w, r, id)
	case "GET":
		if sub != "" && sub != "/" {
			errResp(w, 404, "not found")
			return
		}
		s.handleListPublishRules(w, r, id)
	case "DELETE":
		portStr := strings.TrimPrefix(sub, "/")
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			errResp(w, 400, "invalid port in path")
			return
		}
		s.handleUnpublish(w, r, id, port)
	default:
		errResp(w, 405, "method not allowed")
	}
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request, sandboxID string) {
	user := UserFromContext(r.Context())
	sb := s.getUserSandbox(w, r, sandboxID)
	if sb == nil {
		return
	}

	var req struct {
		Port  int    `json:"port"`
		Alias string `json:"alias,omitempty"`
	}
	if err := readJSON(r, &req); err != nil {
		errResp(w, 400, "invalid request body")
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		errResp(w, 400, "port must be 1-65535")
		return
	}

	alias := req.Alias
	if alias == "" {
		var err error
		alias, err = generateUniqueAlias(s.store, sb.Name)
		if err != nil {
			errResp(w, 500, "alias generation failed")
			return
		}
	}
	if err := validateAlias(alias); err != nil {
		errResp(w, 400, err.Error())
		return
	}

	rule := store.PublishRule{
		ID:        "pub_" + genID(),
		SandboxID: sb.ID,
		UserID:    user.ID,
		Port:      req.Port,
		Alias:     alias,
	}
	if err := s.store.CreatePublishRule(rule); err != nil {
		if strings.Contains(err.Error(), "already taken") ||
			strings.Contains(err.Error(), "already published") {
			errResp(w, 409, err.Error())
		} else {
			errResp(w, 500, err.Error())
		}
		return
	}

	s.RecordEvent(store.Event{
		Type: "publish.created", UserID: user.ID, SandboxID: sb.ID,
		Meta: map[string]any{"sandbox": sb.Name, "port": req.Port, "alias": alias,
			"url": publishedURL(alias, s.proxyZone, s.publicProxyAddr)},
	})
	writeJSON(w, 201, map[string]interface{}{
		"id":         rule.ID,
		"sandbox_id": sb.ID,
		"port":       rule.Port,
		"alias":      alias,
		"url":        publishedURL(alias, s.proxyZone, s.publicProxyAddr),
		"created_at": rule.CreatedAt,
	})
}

func (s *Server) handleListPublishRules(w http.ResponseWriter, r *http.Request, sandboxID string) {
	sb := s.getUserSandbox(w, r, sandboxID)
	if sb == nil {
		return
	}
	rules, err := s.store.ListPublishRules(sb.ID)
	if err != nil {
		errResp(w, 500, err.Error())
		return
	}
	type ruleResp struct {
		store.PublishRule
		URL string `json:"url"`
	}
	resp := make([]ruleResp, len(rules))
	for i, r := range rules {
		resp[i] = ruleResp{PublishRule: r, URL: publishedURL(r.Alias, s.proxyZone, s.publicProxyAddr)}
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleUnpublish(w http.ResponseWriter, r *http.Request, sandboxID string, port int) {
	user := UserFromContext(r.Context())
	sb := s.getUserSandbox(w, r, sandboxID)
	if sb == nil {
		return
	}
	// Look up alias before deleting to invalidate route cache.
	rules, _ := s.store.ListPublishRules(sb.ID)
	if err := s.store.DeletePublishRule(user.ID, sb.ID, port); err != nil {
		errResp(w, 404, err.Error())
		return
	}
	for _, r := range rules {
		if r.Port == port {
			if s.publicProxy != nil {
				s.publicProxy.routeCache.Invalidate(r.Alias)
			}
			s.RecordEvent(store.Event{
				Type: "publish.deleted", UserID: user.ID, SandboxID: sb.ID,
				Meta: map[string]any{"sandbox": sb.Name, "port": port, "alias": r.Alias},
			})
			break
		}
	}
	w.WriteHeader(204)
}
