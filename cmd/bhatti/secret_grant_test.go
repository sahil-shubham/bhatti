package main

import (
	"reflect"
	"testing"
)

func TestParseSecretGrantFlag(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		secret    string
		hosts     []string
		wantError bool
	}{
		{name: "single host", input: "GH_TOKEN@api.github.com", secret: "GH_TOKEN", hosts: []string{"api.github.com"}},
		{name: "multiple hosts", input: "GH_TOKEN@api.github.com,uploads.github.com", secret: "GH_TOKEN", hosts: []string{"api.github.com", "uploads.github.com"}},
		{name: "wildcard host", input: "KEY@*.example.com", secret: "KEY", hosts: []string{"*.example.com"}},
		{name: "missing at", input: "GH_TOKEN", wantError: true},
		{name: "empty name", input: "@api.github.com", wantError: true},
		{name: "empty hosts", input: "GH_TOKEN@", wantError: true},
		{name: "trailing comma", input: "GH_TOKEN@api.github.com,", wantError: true},
		{name: "empty middle host", input: "GH_TOKEN@api.github.com,,uploads.github.com", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret, hosts, err := parseSecretGrantFlag(tt.input)
			if (err != nil) != tt.wantError {
				t.Fatalf("parseSecretGrantFlag(%q) error = %v, wantError %t", tt.input, err, tt.wantError)
			}
			if !tt.wantError && (secret != tt.secret || !reflect.DeepEqual(hosts, tt.hosts)) {
				t.Errorf("parseSecretGrantFlag(%q) = (%q, %v), want (%q, %v)", tt.input, secret, hosts, tt.secret, tt.hosts)
			}
		})
	}
}
