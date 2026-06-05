package server

import (
	"reflect"
	"strings"
	"testing"
)

// validateLabel + validateLabels + parseLabelQueryParams — G1.6 server
// layer. These are pure functions; HTTP-end-to-end coverage lives in
// the broader sandbox-handler tests.

func TestValidateLabel_AcceptsCommonShapes(t *testing.T) {
	ok := []struct{ k, v string }{
		{"pool", "workers"},
		{"env", "prod"},
		{"team-a", "platform"},
		{"app.kubernetes.io/name", "myapp"},   // namespaced
		{"version", "v1.2.3"},                  // dots in value
		{"experimental", ""},                   // empty value (boolean)
		{"a", "b"},                             // single char
		{"a_b.c-d", "x.y_z-1"},
	}
	for _, tc := range ok {
		if err := validateLabel(tc.k, tc.v); err != nil {
			t.Errorf("validateLabel(%q, %q) should be valid: %v", tc.k, tc.v, err)
		}
	}
}

func TestValidateLabel_RejectsBadShapes(t *testing.T) {
	bad := []struct {
		k, v string
		why  string
	}{
		{"", "v", "empty key"},
		{"-leading", "v", "leading dash"},
		{"trailing-", "v", "trailing dash"},
		{"has spaces", "v", "spaces in key"},
		{"k", "has spaces", "spaces in value"},
		{"k=eq", "v", "= in key"},
		{strings.Repeat("a", 64), "v", "key segment >63 chars"},
		{"k", strings.Repeat("a", maxLabelValueLen+1), "value too long"},
		{"bhatti.sh/owner", "v", "reserved prefix"},
		{"foo/", "v", "trailing slash on namespace"},
		{"/foo", "v", "leading slash"},
	}
	for _, tc := range bad {
		if err := validateLabel(tc.k, tc.v); err == nil {
			t.Errorf("validateLabel(%q, %q) should be invalid (%s)", tc.k, tc.v, tc.why)
		}
	}
}

func TestValidateLabels_CountCap(t *testing.T) {
	labels := map[string]string{}
	for i := 0; i < maxLabelsPerSandbox; i++ {
		labels[string(rune('a'+i))] = "v"
	}
	if err := validateLabels(labels); err != nil {
		t.Fatalf("at cap (%d): %v", maxLabelsPerSandbox, err)
	}
	labels["overflow"] = "v"
	if err := validateLabels(labels); err == nil {
		t.Fatalf("over cap (%d) should fail", maxLabelsPerSandbox+1)
	}
}

func TestParseLabelQueryParams(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    map[string]string
		wantErr bool
	}{
		{"empty", nil, nil, false},
		{"single", []string{"pool=workers"}, map[string]string{"pool": "workers"}, false},
		{"multiple", []string{"pool=workers", "env=prod"},
			map[string]string{"pool": "workers", "env": "prod"}, false},
		{"empty value", []string{"experimental="},
			map[string]string{"experimental": ""}, false},
		{"value contains equals", []string{"q=a=b=c"},
			map[string]string{"q": "a=b=c"}, false},
		{"missing equals", []string{"poolworkers"}, nil, true},
		{"empty key", []string{"=value"}, nil, true},
		{"reserved prefix", []string{"bhatti.sh/own=me"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLabelQueryParams(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestValidateLabelKeys_BasicAndReserved(t *testing.T) {
	if err := validateLabelKeys([]string{"pool", "env"}); err != nil {
		t.Fatalf("valid keys: %v", err)
	}
	if err := validateLabelKeys([]string{"bhatti.sh/owner"}); err == nil {
		t.Fatal("reserved-prefix key should fail")
	}
	if err := validateLabelKeys([]string{""}); err == nil {
		t.Fatal("empty key should fail")
	}
}
