package main

import (
	"strings"
	"testing"
)

func TestCreateForkRejectsExplicitNetworkFlagsBeforeRequest(t *testing.T) {
	for _, tc := range []struct{ flag, value string }{
		{"net", "true"}, {"egress", "deny"}, {"allow-siblings", "true"},
		{"allow-host", "api.example.com"}, {"allow-cidr", "1.1.1.1/32"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			flags := createCmd.Flags()
			from := flags.Lookup("from")
			override := flags.Lookup(tc.flag)
			oldFrom, oldOverride := from.Value.String(), override.Value.String()
			oldFromChanged, oldOverrideChanged := from.Changed, override.Changed
			t.Cleanup(func() {
				flags.Set("from", oldFrom)
				flags.Set(tc.flag, oldOverride)
				from.Changed, override.Changed = oldFromChanged, oldOverrideChanged
			})
			if err := flags.Set("from", "source"); err != nil {
				t.Fatal(err)
			}
			if err := flags.Set(tc.flag, tc.value); err != nil {
				t.Fatal(err)
			}
			if err := createCmd.RunE(createCmd, nil); err == nil || !strings.Contains(err.Error(), "inherits the source's") {
				t.Fatalf("fork --%s = %v, want local policy inheritance error", tc.flag, err)
			}
		})
	}
}
