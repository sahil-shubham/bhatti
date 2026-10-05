package main

import (
	"math"
	"sort"
	"time"
)

// Sample records one attempted operation, including unsuccessful attempts.
type Sample struct {
	Op        string             `json:"op"`
	StartedAt time.Time          `json:"started_at"`
	LatencyMS float64            `json:"latency_ms"`
	Outcome   string             `json:"outcome"` // ok, err, 429, or timeout
	Error     string             `json:"error,omitempty"`
	Values    map[string]float64 `json:"values,omitempty"`
}

// OpSummary reports outcomes for all attempts and latency for successful ones.
type OpSummary struct {
	Count             int     `json:"count"`
	OK                int     `json:"ok"`
	Errors            int     `json:"errors"`
	RateLimited       int     `json:"rate_limited"`
	Timeouts          int     `json:"timeouts"`
	P50MS             float64 `json:"p50_ms"`
	P90MS             float64 `json:"p90_ms"`
	P95MS             float64 `json:"p95_ms"`
	P99MS             float64 `json:"p99_ms"`
	MaxMS             float64 `json:"max_ms"`
	MeanMS            float64 `json:"mean_ms"`
	ErrRate           float64 `json:"err_rate"`
	ThroughputOpsPerS float64 `json:"throughput_ops_per_s"`
}

func summarize(samples []Sample, duration time.Duration) map[string]OpSummary {
	type accumulator struct {
		summary    OpSummary
		latencies  []float64
		latencySum float64
	}
	groups := make(map[string]*accumulator)
	for _, sample := range samples {
		group := groups[sample.Op]
		if group == nil {
			group = &accumulator{}
			groups[sample.Op] = group
		}
		group.summary.Count++
		switch sample.Outcome {
		case "ok":
			group.summary.OK++
			group.latencies = append(group.latencies, sample.LatencyMS)
			group.latencySum += sample.LatencyMS
		case "429":
			group.summary.RateLimited++
		case "timeout":
			group.summary.Timeouts++
		default:
			// Unrecognized failures still count as errors, never as successful latency.
			group.summary.Errors++
		}
	}

	result := make(map[string]OpSummary, len(groups))
	for op, group := range groups {
		summary := group.summary
		summary.ErrRate = float64(summary.Errors+summary.RateLimited+summary.Timeouts) / float64(summary.Count)
		if duration > 0 {
			summary.ThroughputOpsPerS = float64(summary.Count) / duration.Seconds()
		}
		if len(group.latencies) > 0 {
			sort.Float64s(group.latencies)
			percentile := func(p float64) float64 {
				rank := int(math.Ceil(p * float64(len(group.latencies))))
				return group.latencies[rank-1]
			}
			summary.P50MS = percentile(0.50)
			summary.P90MS = percentile(0.90)
			summary.P95MS = percentile(0.95)
			summary.P99MS = percentile(0.99)
			summary.MaxMS = group.latencies[len(group.latencies)-1]
			summary.MeanMS = group.latencySum / float64(len(group.latencies))
		}
		result[op] = summary
	}
	return result
}
