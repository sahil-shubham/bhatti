package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestForkRejectsExplicitNetworkPolicy(t *testing.T) {
	for _, policy := range []map[string]any{
		{"siblings": "allow"},
		{"default": "public"},
		{"allow_hosts": []string{"api.example.com"}},
		{"allow_cidrs": []string{"1.1.1.1/32"}},
		{},
	} {
		t.Run("policy", func(t *testing.T) {
			srv, ts := setup(t)
			resp := doReq(t, ts, http.MethodPost, "/sandboxes", map[string]any{
				"name": "child", "from": "source", "net_policy": policy,
			})
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "inherits the source's") {
				t.Fatalf("fork with explicit policy = %d %s, want 400 inheritance error", resp.StatusCode, body)
			}
			if srv.engine.(*mockEngine).nextID.Load() != 0 {
				t.Fatal("rejected fork reached engine")
			}
		})
	}
}
