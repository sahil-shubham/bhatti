//go:build linux

package firecracker

import "testing"

func TestStateStr(t *testing.T) {
	m := map[string]interface{}{
		"present": "hello",
		"number":  42,
	}
	if got := stateStr(m, "present"); got != "hello" {
		t.Errorf("present: %q, want 'hello'", got)
	}
	if got := stateStr(m, "missing"); got != "" {
		t.Errorf("missing: %q, want ''", got)
	}
	if got := stateStr(m, "number"); got != "" {
		t.Errorf("wrong type: %q, want ''", got)
	}
}

func TestStateInt64(t *testing.T) {
	cases := []struct {
		name string
		val  interface{}
		want int64
	}{
		{"int", int(42), 42},
		{"int64", int64(99), 99},
		{"float64", float64(3.14), 3},  // truncates
		{"uint32", uint32(7), 7},
		{"nil", nil, 0},
		{"string", "nope", 0},
	}
	for _, tc := range cases {
		m := map[string]interface{}{"k": tc.val}
		if got := stateInt64(m, "k"); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	// Missing key
	if got := stateInt64(map[string]interface{}{}, "k"); got != 0 {
		t.Errorf("missing: %d, want 0", got)
	}
}

func TestStateUint32(t *testing.T) {
	cases := []struct {
		name string
		val  interface{}
		want uint32
	}{
		{"int", int(42), 42},
		{"int64", int64(99), 99},
		{"float64", float64(256.9), 256},
		{"uint32", uint32(7), 7},
		{"nil", nil, 0},
	}
	for _, tc := range cases {
		m := map[string]interface{}{"k": tc.val}
		if got := stateUint32(m, "k"); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestStateBool(t *testing.T) {
	cases := []struct {
		name string
		val  interface{}
		want bool
	}{
		{"true", true, true},
		{"false", false, false},
		{"int 1", int(1), true},
		{"int 0", int(0), false},
		{"float64 1", float64(1), true},
		{"float64 0", float64(0), false},
		{"nil", nil, false},
		{"string", "true", false}, // strings not supported
	}
	for _, tc := range cases {
		m := map[string]interface{}{"k": tc.val}
		if got := stateBool(m, "k"); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	// Missing key
	if got := stateBool(map[string]interface{}{}, "k"); got != false {
		t.Errorf("missing: %v, want false", got)
	}
}
