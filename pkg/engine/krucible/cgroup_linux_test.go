//go:build linux

package krucible

import (
	"errors"
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
