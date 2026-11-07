//go:build linux

package krucible

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestMapBusReferenceErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"org.freedesktop.systemd1.NoSuchUnit", errNoSuchUnit},
		{"org.freedesktop.systemd1.NotReferenced", errUnitNotReferenced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := mapBusError(dbus.Error{Name: tc.name}); !errors.Is(err, tc.want) {
				t.Fatalf("release response = %v, want %v", err, tc.want)
			}
		})
	}
	denied := dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"}
	if err := mapBusError(denied); errors.Is(err, errNoSuchUnit) || errors.Is(err, errUnitNotReferenced) {
		t.Fatalf("a rejected release was treated as completed: %v", err)
	}
}

func TestParseProcStartTime(t *testing.T) {
	fields := []string{"R"}
	for field := 4; field < 22; field++ {
		fields = append(fields, fmt.Sprint(field))
	}
	prefix := "123 (a comm with spaces ) and\nparentheses)) " + strings.Join(fields, " ") + " "
	start, err := parseProcStartTime([]byte(prefix + "987654 23 24\n"))
	if err != nil || start != 987654 {
		t.Fatalf("start time = %d (%v), want field 22, not an adjacent field", start, err)
	}
	for _, bad := range []string{
		"123 no-parentheses R 1",
		"123 (comm) R 1 2",
		prefix,
		prefix + "-1",
		prefix + "18446744073709551616",
		prefix + "not-a-number",
	} {
		if start, err := parseProcStartTime([]byte(bad)); err == nil {
			t.Errorf("malformed stat returned start time %d: %q", start, bad)
		}
	}
}
