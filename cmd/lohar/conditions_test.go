//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeUnitWithConditions builds a synthetic Unit whose [Unit] section
// has the given Condition*= entries. Each entry is "Key=Value" or
// "Key=!Value" for the negated form.
func makeUnitWithConditions(entries ...string) *Unit {
	kvs := []kvPair{}
	for _, e := range entries {
		idx := strings.Index(e, "=")
		if idx < 0 {
			continue
		}
		kvs = append(kvs, kvPair{key: e[:idx], value: e[idx+1:]})
	}
	return &Unit{
		Sections: serviceFile{sections: map[string][]kvPair{
			"Unit": kvs,
		}},
	}
}

func TestEvaluateConditionsPassesByDefault(t *testing.T) {
	// A unit with no Condition*= directives passes.
	u := makeUnitWithConditions()
	ok, reason := evaluateConditions(u)
	if !ok {
		t.Errorf("no-conditions unit failed: %s", reason)
	}
}

func TestConditionPathExists(t *testing.T) {
	dir := t.TempDir()
	exists := filepath.Join(dir, "sentinel")
	os.WriteFile(exists, []byte{}, 0644)
	missing := filepath.Join(dir, "ghost")

	cases := []struct {
		name      string
		entry     string
		wantPass  bool
		wantInMsg string
	}{
		{"path-exists-positive", "ConditionPathExists=" + exists, true, ""},
		{"path-exists-positive-fails", "ConditionPathExists=" + missing, false, "absent"},
		{"path-exists-negated-passes-when-absent", "ConditionPathExists=!" + missing, true, ""},
		{"path-exists-negated-fails-when-present", "ConditionPathExists=!" + exists, false, "exists"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := makeUnitWithConditions(c.entry)
			ok, reason := evaluateConditions(u)
			if ok != c.wantPass {
				t.Errorf("got pass=%v reason=%q, want pass=%v", ok, reason, c.wantPass)
			}
			if c.wantInMsg != "" && !strings.Contains(reason, c.wantInMsg) {
				t.Errorf("reason %q should contain %q", reason, c.wantInMsg)
			}
		})
	}
}

func TestConditionPathIsDirectory(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "subdir")
	os.MkdirAll(subdir, 0755)
	file := filepath.Join(dir, "regular")
	os.WriteFile(file, []byte{}, 0644)

	cases := []struct {
		entry    string
		wantPass bool
	}{
		{"ConditionPathIsDirectory=" + subdir, true},
		{"ConditionPathIsDirectory=" + file, false},
		{"ConditionPathIsDirectory=" + filepath.Join(dir, "missing"), false},
		{"ConditionPathIsDirectory=!" + file, true},
		{"ConditionPathIsDirectory=!" + subdir, false},
	}
	for _, c := range cases {
		u := makeUnitWithConditions(c.entry)
		ok, _ := evaluateConditions(u)
		if ok != c.wantPass {
			t.Errorf("%q: got %v, want %v", c.entry, ok, c.wantPass)
		}
	}
}

func TestConditionDirectoryNotEmpty(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	os.MkdirAll(empty, 0755)
	full := filepath.Join(dir, "full")
	os.MkdirAll(full, 0755)
	os.WriteFile(filepath.Join(full, "thing"), []byte("x"), 0644)

	cases := []struct {
		entry    string
		wantPass bool
	}{
		{"ConditionDirectoryNotEmpty=" + full, true},
		{"ConditionDirectoryNotEmpty=" + empty, false},
		{"ConditionDirectoryNotEmpty=" + filepath.Join(dir, "missing"), false},
	}
	for _, c := range cases {
		u := makeUnitWithConditions(c.entry)
		ok, _ := evaluateConditions(u)
		if ok != c.wantPass {
			t.Errorf("%q: got %v, want %v", c.entry, ok, c.wantPass)
		}
	}
}

func TestConditionFileNotEmpty(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, []byte{}, 0644)
	full := filepath.Join(dir, "full")
	os.WriteFile(full, []byte("content"), 0644)

	cases := []struct {
		entry    string
		wantPass bool
	}{
		{"ConditionFileNotEmpty=" + full, true},
		{"ConditionFileNotEmpty=" + empty, false},
		{"ConditionFileNotEmpty=" + filepath.Join(dir, "missing"), false},
	}
	for _, c := range cases {
		u := makeUnitWithConditions(c.entry)
		ok, _ := evaluateConditions(u)
		if ok != c.wantPass {
			t.Errorf("%q: got %v, want %v", c.entry, ok, c.wantPass)
		}
	}
}

func TestConditionMultipleAllPass(t *testing.T) {
	// Multiple Condition*= directives: all must pass for the unit to
	// run. If any fails, the unit is skipped.
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	os.WriteFile(a, []byte{}, 0644)
	os.WriteFile(b, []byte("x"), 0644)

	// Both conditions satisfied.
	u := makeUnitWithConditions(
		"ConditionPathExists="+a,
		"ConditionFileNotEmpty="+b,
	)
	if ok, reason := evaluateConditions(u); !ok {
		t.Errorf("both-pass: got fail %q", reason)
	}

	// One condition fails -> overall fails.
	missing := filepath.Join(dir, "missing")
	u2 := makeUnitWithConditions(
		"ConditionPathExists="+a,
		"ConditionPathExists="+missing,
	)
	if ok, _ := evaluateConditions(u2); ok {
		t.Error("one-fails: should have failed")
	}
}

func TestConditionNilUnit(t *testing.T) {
	// Defensive: an unresolved unit (nil) should not crash.
	if ok, _ := evaluateConditions(nil); !ok {
		t.Error("nil unit should pass (no conditions to check)")
	}
}

func TestStripNegation(t *testing.T) {
	cases := []struct {
		in       string
		wantPath string
		wantNeg  bool
	}{
		{"/etc/foo", "/etc/foo", false},
		{"!/etc/foo", "/etc/foo", true},
		{"  !/etc/foo  ", "/etc/foo", true},
		{"!  /etc/foo", "/etc/foo", true},
		{"", "", false},
		{"!", "", true},
	}
	for _, c := range cases {
		path, neg := stripNegation(c.in)
		if path != c.wantPath || neg != c.wantNeg {
			t.Errorf("stripNegation(%q) = (%q, %v), want (%q, %v)",
				c.in, path, neg, c.wantPath, c.wantNeg)
		}
	}
}
