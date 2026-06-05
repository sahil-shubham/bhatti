package server

import (
	"io"
	"net/url"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

// End-to-end HTTP tests for the label surface (G1.6). Run against
// the same mock-engine setup the rest of the handler tests use.

// TestSandboxLabels_CreateAndList covers POST with labels and GET
// without filter — labels survive the round-trip.
func TestSandboxLabels_CreateAndList(t *testing.T) {
	_, ts := setup(t)

	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"name":   uniqueName(t, "lab"),
		"labels": map[string]string{"pool": "workers", "env": "prod"},
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create with labels: got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	t.Cleanup(func() { doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil) })

	if sb.Labels["pool"] != "workers" || sb.Labels["env"] != "prod" {
		t.Fatalf("labels not in create response: %v", sb.Labels)
	}

	// GET should also return the labels.
	resp = doReq(t, ts, "GET", "/sandboxes", nil)
	var list []map[string]any
	decodeJSON(t, resp, &list)
	if len(list) == 0 {
		t.Fatal("expected at least one sandbox")
	}
	found := false
	for _, s := range list {
		if s["id"] == sb.ID {
			found = true
			labels, ok := s["labels"].(map[string]any)
			if !ok || labels["pool"] != "workers" {
				t.Fatalf("labels missing or wrong on list: %v", s["labels"])
			}
		}
	}
	if !found {
		t.Fatal("sandbox not in list")
	}
}

// TestSandboxLabels_GetFilter exercises ?label=k=v query params.
func TestSandboxLabels_GetFilter(t *testing.T) {
	_, ts := setup(t)

	// Three sandboxes with overlapping labels.
	mk := func(name string, labels map[string]string) string {
		resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
			"name": name, "labels": labels,
		})
		if resp.StatusCode != 201 {
			t.Fatalf("create %s: %d", name, resp.StatusCode)
		}
		var sb store.Sandbox
		decodeJSON(t, resp, &sb)
		t.Cleanup(func() { doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil) })
		return sb.ID
	}

	idWorkProd := mk(uniqueName(t, "wp"), map[string]string{"pool": "workers", "env": "prod"})
	idWorkStag := mk(uniqueName(t, "ws"), map[string]string{"pool": "workers", "env": "staging"})
	idDbProd := mk(uniqueName(t, "dp"), map[string]string{"pool": "database", "env": "prod"})

	// Helper: GET with arbitrary query, decode, return set of IDs.
	fetchIDs := func(q string) map[string]bool {
		resp := doReq(t, ts, "GET", "/sandboxes?"+q, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("GET ?%s: %d", q, resp.StatusCode)
		}
		var list []map[string]any
		decodeJSON(t, resp, &list)
		ids := map[string]bool{}
		for _, s := range list {
			ids[s["id"].(string)] = true
		}
		return ids
	}

	tests := []struct {
		name   string
		query  url.Values
		expect []string
	}{
		{"pool=workers", url.Values{"label": []string{"pool=workers"}}, []string{idWorkProd, idWorkStag}},
		{"pool=workers AND env=prod", url.Values{"label": []string{"pool=workers", "env=prod"}}, []string{idWorkProd}},
		{"env=prod", url.Values{"label": []string{"env=prod"}}, []string{idWorkProd, idDbProd}},
		{"no match", url.Values{"label": []string{"pool=workers", "env=qa"}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fetchIDs(tt.query.Encode())
			if len(got) != len(tt.expect) {
				t.Fatalf("expected %v (%d), got %v (%d)", tt.expect, len(tt.expect), got, len(got))
			}
			for _, id := range tt.expect {
				if !got[id] {
					t.Errorf("expected %s in result", id)
				}
			}
		})
	}
}

// TestSandboxLabels_PatchMerge covers labels_add + labels_remove via
// PATCH. The merge happens in the store; this is the HTTP wiring test.
func TestSandboxLabels_PatchMerge(t *testing.T) {
	_, ts := setup(t)

	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"name":   uniqueName(t, "p"),
		"labels": map[string]string{"pool": "workers", "env": "prod", "tier": "small"},
	})
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	t.Cleanup(func() { doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil) })

	patch := doReq(t, ts, "PATCH", "/sandboxes/"+sb.ID, map[string]any{
		"labels_add":    map[string]string{"env": "staging", "team": "platform"},
		"labels_remove": []string{"tier"},
	})
	if patch.StatusCode != 200 {
		body, _ := io.ReadAll(patch.Body)
		t.Fatalf("PATCH: %d: %s", patch.StatusCode, body)
	}

	// Re-fetch and verify merged state.
	get := doReq(t, ts, "GET", "/sandboxes/"+sb.ID, nil)
	var after store.Sandbox
	decodeJSON(t, get, &after)

	want := map[string]string{"pool": "workers", "env": "staging", "team": "platform"}
	if len(after.Labels) != len(want) {
		t.Fatalf("got %v, want %v", after.Labels, want)
	}
	for k, v := range want {
		if after.Labels[k] != v {
			t.Errorf("label %q: got %q want %q", k, after.Labels[k], v)
		}
	}
	if _, ok := after.Labels["tier"]; ok {
		t.Errorf("tier should be removed: %v", after.Labels)
	}
}

// TestSandboxLabels_RejectInvalidOnCreate ensures the validation layer
// is wired: a POST with a bad label returns 400 before the sandbox is
// created.
func TestSandboxLabels_RejectInvalidOnCreate(t *testing.T) {
	_, ts := setup(t)

	tests := []struct {
		name   string
		labels map[string]string
	}{
		{"empty key", map[string]string{"": "v"}},
		{"reserved prefix", map[string]string{"bhatti.sh/owner": "v"}},
		{"bad chars in key", map[string]string{"has spaces": "v"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
				"name":   uniqueName(t, "bad"),
				"labels": tt.labels,
			})
			if resp.StatusCode != 400 {
				t.Fatalf("expected 400, got %d", resp.StatusCode)
			}
		})
	}
}

// TestSandboxLabels_FilterRejectsReservedPrefix protects against a
// future caller probing system labels via the public query API.
func TestSandboxLabels_FilterRejectsReservedPrefix(t *testing.T) {
	_, ts := setup(t)
	resp := doReq(t, ts, "GET", "/sandboxes?label=bhatti.sh%2Fowner=me", nil)
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 for reserved-prefix filter, got %d", resp.StatusCode)
	}
}
