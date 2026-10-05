package main

import (
	"testing"
	"time"
)

func TestParseCurlMetricsRequiresFiniteNumericFields(t *testing.T) {
	got, err := parseCurlMetrics("progress\nBHT_DL size_download=10485760 speed_download=22096124 time_total=0.474552\n", "BHT_DL", "size_download", "speed_download", "time_total")
	if err != nil || got["size_download"] != 10485760 || got["speed_download"] != 22096124 || got["time_total"] != 0.474552 {
		t.Fatalf("parsed guest curl output = %#v, err %v", got, err)
	}
	for _, bad := range []string{
		"size_download=10485760 speed_download=10 time_total=1", // no marker
		"BHT_DL size_download=0 speed_download=NaN time_total=1",
		"BHT_DL size_download=1 speed_download=10", // incomplete
	} {
		if _, err := parseCurlMetrics(bad, "BHT_DL", "size_download", "speed_download", "time_total"); err == nil {
			t.Errorf("accepted invalid guest curl output %q", bad)
		}
	}
}

func TestParseConnectProbePreservesGuestNanoseconds(t *testing.T) {
	got, err := parseConnectProbe("BHT_CONNECT start_ns=1791192389904694301 time_connect=0.005986 curl_exit=0")
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Unix(0, 1791192389904694301); !got.start.Equal(want) {
		t.Errorf("guest start = %s, want %s", got.start, want)
	}
	if got.ms != 5.986 || got.curlExit != 0 {
		t.Fatalf("probe = %#v", got)
	}
	for _, bad := range []string{
		"BHT_CONNECT start_ns=0 time_connect=0.01 curl_exit=0",
		"BHT_CONNECT start_ns=1791192389904694301 time_connect=NaN curl_exit=0",
		"BHT_CONNECT start_ns=1791192389904694301 time_connect=0.01 curl_exit=256",
	} {
		if _, err := parseConnectProbe(bad); err == nil {
			t.Errorf("accepted invalid guest TCP sample %q", bad)
		}
	}
}
