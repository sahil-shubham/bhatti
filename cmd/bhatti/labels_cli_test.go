package main

import (
	"reflect"
	"testing"
)

// CLI-layer label helpers — G1.6. The HTTP-level validation runs on the
// server; the client just parses --label flag strings into a map and
// formats label maps for tabular output.

func TestParseLabelFlag(t *testing.T) {
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
		{"last value wins on duplicate key", []string{"k=v1", "k=v2"},
			map[string]string{"k": "v2"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLabelFlag(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestFormatLabels(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		want string
	}{
		{"empty", nil, "-"},
		{"explicit empty", map[string]string{}, "-"},
		{"single", map[string]string{"pool": "workers"}, "pool=workers"},
		// Ordering is alphabetical for determinism. The output must
		// not depend on map iteration order.
		{"multi sorted", map[string]string{"env": "prod", "pool": "workers", "team": "platform"},
			"env=prod,pool=workers,team=platform"},
		{"value with dash", map[string]string{"version": "v1.2.3"}, "version=v1.2.3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatLabels(tt.in)
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}
