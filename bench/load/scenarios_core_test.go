package main

import "testing"

func TestDensityPointOnlyAttributesCompleteBenchPSS(t *testing.T) {
	start := HostSample{MemAvailableMB: 1200}
	for _, tc := range []struct {
		name string
		at   HostSample
		want any
	}{
		{"complete bench count", HostSample{Scope: "bench_only", VMMCount: 3, PSSComplete: true, VMMPSSMB: 180, MemAvailableMB: 1100}, float64(60)},
		{"partial PSS", HostSample{Scope: "bench_only", VMMCount: 3, VMMPSSMB: 120, MemAvailableMB: 1100}, nil},
		{"unattributed guests", HostSample{Scope: "host_total", VMMCount: 3, PSSComplete: true, VMMPSSMB: 180, MemAvailableMB: 1100}, nil},
		{"unrelated bench VM", HostSample{Scope: "bench_only", VMMCount: 4, PSSComplete: true, VMMPSSMB: 240, MemAvailableMB: 1100}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := densityPoint(start, tc.at, "hot", 3)
			if got["vmm_pss_per_sandbox_mb"] != tc.want {
				t.Fatalf("per-sandbox PSS = %v, want %v", got["vmm_pss_per_sandbox_mb"], tc.want)
			}
			if got["host_mem_delta_mb"] != float64(100) {
				t.Fatalf("host-wide RAM delta = %v, want 100", got["host_mem_delta_mb"])
			}
			if got["scope"] != tc.at.Scope || got["pss_complete"] != tc.at.PSSComplete {
				t.Fatalf("attribution metadata not preserved: %#v", got)
			}
		})
	}
}
