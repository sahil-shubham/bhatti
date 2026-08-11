package krucible

import (
	"context"
	"os"
	"testing"
)

// TestHelperStatsReadsPid proves the ps-based sampler parses rss/cpu for a real
// pid (the running test process) — VM-free, so it runs in normal CI. A broken
// ps invocation or parse yields zero RSS, which fails here.
func TestHelperStatsReadsPid(t *testing.T) {
	st := helperStats(context.Background(), os.Getpid())
	if st.RSSBytes <= 0 {
		t.Fatalf("helperStats RSS = %d bytes, want > 0 for the running test process", st.RSSBytes)
	}
	if st.CPUPct < 0 {
		t.Fatalf("helperStats CPU%% = %v, want >= 0", st.CPUPct)
	}
}
