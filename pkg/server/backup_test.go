package server

import (
	"testing"
	"time"
)

func TestCronMatch(t *testing.T) {
	tests := []struct {
		name string
		cron string
		time time.Time
		want bool
	}{
		{
			name: "every minute",
			cron: "* * * * *",
			time: time.Date(2026, 4, 2, 15, 30, 0, 0, time.UTC),
			want: true,
		},
		{
			name: "specific minute and hour",
			cron: "0 3 * * *",
			time: time.Date(2026, 4, 2, 3, 0, 0, 0, time.UTC),
			want: true,
		},
		{
			name: "wrong minute",
			cron: "0 3 * * *",
			time: time.Date(2026, 4, 2, 3, 1, 0, 0, time.UTC),
			want: false,
		},
		{
			name: "wrong hour",
			cron: "0 3 * * *",
			time: time.Date(2026, 4, 2, 4, 0, 0, 0, time.UTC),
			want: false,
		},
		{
			name: "every 6 hours at minute 0",
			cron: "0 6 * * *",
			time: time.Date(2026, 4, 2, 6, 0, 0, 0, time.UTC),
			want: true,
		},
		{
			name: "specific day of week (Wednesday=3)",
			cron: "0 3 * * 3",
			time: time.Date(2026, 4, 1, 3, 0, 0, 0, time.UTC), // Wednesday
			want: true,
		},
		{
			name: "wrong day of week",
			cron: "0 3 * * 3",
			time: time.Date(2026, 4, 2, 3, 0, 0, 0, time.UTC), // Thursday
			want: false,
		},
		{
			name: "all fields specified",
			cron: "30 14 2 4 *",
			time: time.Date(2026, 4, 2, 14, 30, 0, 0, time.UTC),
			want: true,
		},
		{
			name: "invalid cron - too few fields",
			cron: "0 3 *",
			time: time.Date(2026, 4, 2, 3, 0, 0, 0, time.UTC),
			want: false,
		},
		{
			name: "invalid cron - bad number",
			cron: "abc 3 * * *",
			time: time.Date(2026, 4, 2, 3, 0, 0, 0, time.UTC),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cronMatch(tt.cron, tt.time)
			if got != tt.want {
				t.Errorf("cronMatch(%q, %v) = %v, want %v", tt.cron, tt.time, got, tt.want)
			}
		})
	}
}
