package main

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestSummarizeEmpty(t *testing.T) {
	for _, samples := range [][]Sample{nil, {}} {
		got := summarize(samples, time.Second)
		if got == nil || len(got) != 0 {
			t.Fatalf("summarize(%v) = %#v; want an empty map", samples, got)
		}
	}
}

func TestSummarizeNearestRankAndInputOrder(t *testing.T) {
	tests := []struct {
		name      string
		latencies []float64
		want      OpSummary
	}{
		{
			name:      "odd unsorted",
			latencies: []float64{30, 10, 50, 20, 40},
			want: OpSummary{
				Count: 5, OK: 5, P50MS: 30, P90MS: 50, P95MS: 50,
				P99MS: 50, MaxMS: 50, MeanMS: 30, ThroughputOpsPerS: 2.5,
			},
		},
		{
			name:      "even unsorted",
			latencies: []float64{80, 20, 40, 60},
			want: OpSummary{
				Count: 4, OK: 4, P50MS: 40, P90MS: 80, P95MS: 80,
				P99MS: 80, MaxMS: 80, MeanMS: 50, ThroughputOpsPerS: 2,
			},
		},
		{
			name:      "even sorted",
			latencies: []float64{20, 40, 60, 80},
			want: OpSummary{
				Count: 4, OK: 4, P50MS: 40, P90MS: 80, P95MS: 80,
				P99MS: 80, MaxMS: 80, MeanMS: 50, ThroughputOpsPerS: 2,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			samples := make([]Sample, len(tt.latencies))
			for i, latency := range tt.latencies {
				samples[i] = Sample{Op: "create", Outcome: "ok", LatencyMS: latency}
			}
			got := summarize(samples, 2*time.Second)
			if !reflect.DeepEqual(got, map[string]OpSummary{"create": tt.want}) {
				t.Fatalf("summarize(%v) = %#v; want %#v", tt.latencies, got, tt.want)
			}
			for i, latency := range tt.latencies {
				if samples[i].LatencyMS != latency {
					t.Fatalf("summarize changed input at index %d: %v", i, samples[i])
				}
			}
		})
	}
}

func TestSummarizeFailuresExcludedFromLatenciesButCounted(t *testing.T) {
	samples := []Sample{
		{Op: "exec", Outcome: "err", LatencyMS: 1000},
		{Op: "exec", Outcome: "ok", LatencyMS: 4},
		{Op: "exec", Outcome: "429", LatencyMS: 2000},
		{Op: "exec", Outcome: "timeout", LatencyMS: 3000},
		{Op: "exec", Outcome: "ok", LatencyMS: 2},
		{Op: "exec", Outcome: "unexpected", LatencyMS: 4000},
		{Op: "fail-only", Outcome: "timeout", LatencyMS: 9000},
	}
	got := summarize(samples, 2*time.Second)
	want := map[string]OpSummary{
		"exec": {
			Count: 6, OK: 2, Errors: 2, RateLimited: 1, Timeouts: 1,
			P50MS: 2, P90MS: 4, P95MS: 4, P99MS: 4, MaxMS: 4, MeanMS: 3,
			ErrRate: 4.0 / 6, ThroughputOpsPerS: 3,
		},
		"fail-only": {
			Count: 1, Timeouts: 1, ErrRate: 1, ThroughputOpsPerS: 0.5,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summarize failures = %#v; want %#v", got, want)
	}
}

func TestSummarizeZeroDurationAndDistinctOpNames(t *testing.T) {
	names := []string{"", "none", "foo/bar", "foo.bar"}
	var samples []Sample
	for _, name := range names {
		samples = append(samples, Sample{Op: name, Outcome: "ok", LatencyMS: 7})
	}
	for _, duration := range []time.Duration{0, -time.Second} {
		got := summarize(samples, duration)
		if len(got) != len(names) {
			t.Fatalf("summary groups = %#v; want distinct exact op names %q", got, names)
		}
		for _, name := range names {
			if got[name] != (OpSummary{Count: 1, OK: 1, P50MS: 7, P90MS: 7, P95MS: 7, P99MS: 7, MaxMS: 7, MeanMS: 7}) {
				t.Fatalf("summary for %q at duration %s = %#v", name, duration, got[name])
			}
		}
	}
}

func TestSummaryJSONFields(t *testing.T) {
	encoded, err := json.Marshal(Sample{Op: "exec", Outcome: "ok", StartedAt: time.Unix(1, 0), LatencyMS: 1.5})
	if err != nil {
		t.Fatal(err)
	}
	var sampleFields map[string]any
	if err := json.Unmarshal(encoded, &sampleFields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"op", "started_at", "latency_ms", "outcome"} {
		if _, ok := sampleFields[field]; !ok {
			t.Errorf("sample JSON lacks %q: %s", field, encoded)
		}
	}
	for _, field := range []string{"error", "values"} {
		if _, ok := sampleFields[field]; ok {
			t.Errorf("sample JSON unexpectedly includes omitted %q: %s", field, encoded)
		}
	}

	encoded, err = json.Marshal(OpSummary{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"count", "ok", "errors", "rate_limited", "timeouts", "p50_ms", "p90_ms",
		"p95_ms", "p99_ms", "max_ms", "mean_ms", "err_rate", "throughput_ops_per_s",
	} {
		if _, ok := fields[field]; !ok {
			t.Errorf("summary JSON lacks %q: %s", field, encoded)
		}
	}
	if len(fields) != 13 {
		t.Errorf("summary JSON has unexpected fields: %s", encoded)
	}
}
