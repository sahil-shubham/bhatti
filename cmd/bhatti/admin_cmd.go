package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/store"
	"github.com/spf13/cobra"
)

var adminCmd = &cobra.Command{
	Use:   "admin <status|events|metrics>",
	Short: "Observability commands (requires DB access)",
	Long: `Admin commands operate directly on the local SQLite database.
Run on the server, not remotely.`,
	Example: `  bhatti admin status
  bhatti admin events --type thermal --since 24h
  bhatti admin metrics --since 1h`,
}

// --- admin status ---

var adminStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "One-shot system overview",
	RunE: func(cmd *cobra.Command, args []string) error {
		st := openLocalStore()
		defer st.Close()

		if isJSON(cmd) {
			return adminStatusJSON(st)
		}
		return adminStatusHuman(st)
	},
}

func adminStatusJSON(st *store.Store) error {
	latest, _ := st.LatestMetricsSnapshot()
	sums, _ := st.SumMetricsSnapshots(time.Time{})
	events, _ := st.QueryEvents(store.EventFilter{Limit: 10})
	users, _ := st.ListUsers()
	sandboxes, _ := st.ListAllSandboxes()

	outputJSON(map[string]any{
		"gauges":        latest,
		"totals":        sums,
		"recent_events": events,
		"users":         len(users),
		"sandboxes":     len(sandboxes),
	})
	return nil
}

func adminStatusHuman(st *store.Store) error {
	latest, _ := st.LatestMetricsSnapshot()
	sums, _ := st.SumMetricsSnapshots(time.Time{})
	users, _ := st.ListUsers()
	sandboxes, _ := st.ListAllSandboxes()

	// Header + update check
	fmt.Printf("Bhatti %s\n", version)
	if version != "dev" {
		if latestVer := checkLatestRelease(); latestVer != "" {
			normVersion := "v" + strings.TrimPrefix(version, "v")
			normLatest := "v" + strings.TrimPrefix(latestVer, "v")
			if compareVersions(normVersion, normLatest) < 0 {
				fmt.Printf("Update available: %s \u2192 %s (sudo bhatti update)\n", normVersion, normLatest)
			}
		}
	}
	if latest.HostLoad1m > 0 || latest.HostMemTotalMB > 0 {
		fmt.Printf("Host: %.2f load, %.1f / %.1f GB memory\n",
			latest.HostLoad1m,
			float64(latest.HostMemAvailMB)/1024,
			float64(latest.HostMemTotalMB)/1024)
	}
	fmt.Println()

	// Summary
	fmt.Printf("Sandboxes  %d total (%d hot, %d warm, %d cold)\n",
		latest.SandboxesTotal, latest.SandboxesHot, latest.SandboxesWarm, latest.SandboxesCold)
	fmt.Printf("Users      %d total, %d active\n",
		latest.UsersTotal, latest.UsersActive)
	fmt.Printf("API        %s requests, %s errors, %s auth failures\n",
		fmtCount(sums.APIRequests), fmtCount(sums.APIErrors), fmtCount(sums.APIAuthFailures))
	if sums.ProxyRequests > 0 {
		fmt.Printf("Proxy      %s requests, %s errors, %s cold wakes\n",
			fmtCount(sums.ProxyRequests), fmtCount(sums.ProxyErrors), fmtCount(sums.ProxyColdWakes))
	}
	fmt.Println()

	// Per-user sandbox breakdown
	if len(users) > 0 {
		fmt.Printf("%-16s %-12s %s\n", "USER", "SANDBOXES", "STATUS")
		userSandboxes := make(map[string][]store.Sandbox)
		for _, sb := range sandboxes {
			userSandboxes[sb.CreatedBy] = append(userSandboxes[sb.CreatedBy], sb)
		}
		for _, u := range users {
			sbs := userSandboxes[u.ID]
			status := "-"
			if len(sbs) > 0 {
				var running, stopped int
				for _, sb := range sbs {
					if sb.Status == "running" {
						running++
					} else {
						stopped++
					}
				}
				parts := []string{}
				if running > 0 {
					parts = append(parts, fmt.Sprintf("%d running", running))
				}
				if stopped > 0 {
					parts = append(parts, fmt.Sprintf("%d stopped", stopped))
				}
				status = strings.Join(parts, ", ")
			}
			fmt.Printf("%-16s %d/%-9d %s\n", u.Name, len(sbs), u.MaxSandboxes, status)
		}
		fmt.Println()
	}

	// Recent events
	events, _ := st.QueryEvents(store.EventFilter{Limit: 10})
	if len(events) > 0 {
		fmt.Println("RECENT EVENTS")
		for _, e := range events {
			ts := e.Timestamp.Local().Format("15:04:05")
			user := e.UserID
			if user == "" {
				user = "-"
			}
			sandbox := e.SandboxID
			if sandbox == "" {
				sandbox = "-"
			}
			// Try to get sandbox name from meta
			if name, ok := e.Meta["name"].(string); ok && name != "" {
				sandbox = name
			} else if name, ok := e.Meta["sandbox"].(string); ok && name != "" {
				sandbox = name
			}
			details := formatEventDetails(e)
			fmt.Printf("  %s  %-24s %-12s %-16s %s\n", ts, e.Type, user, sandbox, details)
		}
	}

	return nil
}

// --- admin events ---

var adminEventsCmd = &cobra.Command{
	Use:   "events",
	Short: "Query event log",
	RunE: func(cmd *cobra.Command, args []string) error {
		st := openLocalStore()
		defer st.Close()

		f := store.EventFilter{}

		if v, _ := cmd.Flags().GetString("type"); v != "" {
			f.Type = v
		}
		if v, _ := cmd.Flags().GetString("user"); v != "" {
			// Resolve user name to ID
			if u, err := st.GetUserByName(v); err == nil {
				f.UserID = u.ID
			} else {
				f.UserID = v // might be an ID directly
			}
		}
		if v, _ := cmd.Flags().GetString("sandbox"); v != "" {
			f.SandboxID = v
		}
		if v, _ := cmd.Flags().GetString("since"); v != "" {
			f.Since = parseSince(v)
		}
		if v, _ := cmd.Flags().GetInt("limit"); v > 0 {
			f.Limit = v
		}
		if v, _ := cmd.Flags().GetBool("count"); v {
			count, err := st.CountEvents(f)
			if err != nil {
				return err
			}
			fmt.Println(count)
			return nil
		}

		events, err := st.QueryEvents(f)
		if err != nil {
			return err
		}

		if isJSON(cmd) {
			outputJSON(events)
			return nil
		}

		if len(events) == 0 {
			fmt.Println("No events found.")
			return nil
		}

		fmt.Printf("%-20s %-24s %-12s %-16s %s\n", "TS", "TYPE", "USER", "SANDBOX", "DETAILS")
		for _, e := range events {
			ts := e.Timestamp.Local().Format("2006-01-02 15:04:05")
			user := e.UserID
			if user == "" {
				user = "-"
			}
			sandbox := e.SandboxID
			if sandbox == "" {
				sandbox = "-"
			}
			if name, ok := e.Meta["name"].(string); ok && name != "" {
				sandbox = name
			} else if name, ok := e.Meta["sandbox"].(string); ok && name != "" {
				sandbox = name
			}
			details := formatEventDetails(e)
			fmt.Printf("%-20s %-24s %-12s %-16s %s\n", ts, e.Type, user, sandbox, details)
		}
		return nil
	},
}

// --- admin metrics ---

var adminMetricsCmd = &cobra.Command{
	Use:   "metrics",
	Short: "Query metrics snapshots",
	RunE: func(cmd *cobra.Command, args []string) error {
		st := openLocalStore()
		defer st.Close()

		sinceStr, _ := cmd.Flags().GetString("since")
		since := parseSince(sinceStr)
		if since.IsZero() {
			since = time.Now().Add(-1 * time.Hour)
		}

		snaps, err := st.QueryMetricsSnapshots(since, 0)
		if err != nil {
			return err
		}

		if isJSON(cmd) {
			outputJSON(snaps)
			return nil
		}

		if len(snaps) == 0 {
			fmt.Println("No metrics snapshots found.")
			return nil
		}

		// Auto-select bucket size
		duration := time.Since(since)
		bucketSize := 60 * time.Second // 1-min for <2h
		if duration > 2*time.Hour {
			bucketSize = 15 * time.Minute
		}
		if duration > 24*time.Hour {
			bucketSize = 1 * time.Hour
		}

		fmt.Printf("%-18s %8s %4s %10s %6s %4s %5s %5s %6s %10s\n",
			"TIME", "REQ/min", "ERR", "PROXY/min", "WAKES", "HOT", "WARM", "COLD", "LOAD", "MEM_AVAIL")

		type bucket struct {
			start        time.Time
			apiReqs      int64
			apiErrs      int64
			proxyReqs    int64
			proxyWakes   int64
			count        int
			lastSnapshot store.MetricsSnapshot
		}

		var buckets []bucket
		var cur *bucket

		for _, s := range snaps {
			bucketStart := s.Timestamp.Truncate(bucketSize)
			if cur == nil || !cur.start.Equal(bucketStart) {
				buckets = append(buckets, bucket{start: bucketStart})
				cur = &buckets[len(buckets)-1]
			}
			cur.apiReqs += s.APIRequests
			cur.apiErrs += s.APIErrors
			cur.proxyReqs += s.ProxyRequests
			cur.proxyWakes += s.ProxyColdWakes
			cur.count++
			cur.lastSnapshot = s
		}

		for _, b := range buckets {
			minutes := float64(b.count) // each snapshot is ~1 minute
			if minutes == 0 {
				minutes = 1
			}
			fmt.Printf("%-18s %8.0f %4d %10.0f %6d %4d %5d %5d %6.1f %8d MB\n",
				b.start.Local().Format("15:04"),
				float64(b.apiReqs)/minutes,
				b.apiErrs,
				float64(b.proxyReqs)/minutes,
				b.proxyWakes,
				b.lastSnapshot.SandboxesHot,
				b.lastSnapshot.SandboxesWarm,
				b.lastSnapshot.SandboxesCold,
				b.lastSnapshot.HostLoad1m,
				b.lastSnapshot.HostMemAvailMB,
			)
		}
		return nil
	},
}

func init() {
	adminEventsCmd.Flags().String("type", "", "Event type (prefix match: 'thermal' matches thermal.*)")
	adminEventsCmd.Flags().String("user", "", "Filter by user name or ID")
	adminEventsCmd.Flags().String("sandbox", "", "Filter by sandbox name or ID")
	adminEventsCmd.Flags().String("since", "", "Time range (e.g. 24h, 7d, 2026-04-01)")
	adminEventsCmd.Flags().Int("limit", 50, "Max events to return")
	adminEventsCmd.Flags().Bool("count", false, "Return count only")
	adminMetricsCmd.Flags().String("since", "", "Time range (e.g. 1h, 24h, 7d)")

	adminCmd.AddCommand(adminStatusCmd)
	adminCmd.AddCommand(adminEventsCmd)
	adminCmd.AddCommand(adminMetricsCmd)
}

// --- helpers ---

// parseSince converts a string like "24h", "7d", "2026-04-01" to a time.Time.
func parseSince(s string) time.Time {
	if s == "" {
		return time.Time{}
	}

	// Try relative: "24h", "7d", "30m"
	if len(s) > 1 {
		unit := s[len(s)-1]
		numStr := s[:len(s)-1]
		var n float64
		if _, err := fmt.Sscanf(numStr, "%f", &n); err == nil {
			switch unit {
			case 's':
				return time.Now().Add(-time.Duration(n) * time.Second)
			case 'm':
				return time.Now().Add(-time.Duration(n) * time.Minute)
			case 'h':
				return time.Now().Add(-time.Duration(n) * time.Hour)
			case 'd':
				return time.Now().Add(-time.Duration(n) * 24 * time.Hour)
			}
		}
	}

	// Try absolute date formats
	for _, layout := range []string{
		"2006-01-02",
		"2006-01-02T15:04:05",
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}

	return time.Time{}
}

// formatEventDetails produces a one-line summary from the meta JSON.
func formatEventDetails(e store.Event) string {
	switch {
	case strings.HasPrefix(e.Type, "sandbox.created"):
		cpus := fmtAny(e.Meta["cpus"])
		mem := fmtAny(e.Meta["memory_mb"])
		image := fmtAny(e.Meta["image"])
		return fmt.Sprintf("cpus=%s mem=%s image=%s", cpus, mem, image)

	case e.Type == "sandbox.destroyed":
		return fmt.Sprintf("lifetime=%s", fmtDurationSecs(e.Meta["lifetime_s"]))

	case e.Type == "sandbox.stopped" || e.Type == "sandbox.started":
		return fmt.Sprintf("reason=%s", fmtAny(e.Meta["reason"]))

	case e.Type == "thermal.pause":
		return fmt.Sprintf("idle=%ss", fmtAny(e.Meta["idle_s"]))

	case e.Type == "thermal.wake":
		return fmt.Sprintf("from=%s %sms", fmtAny(e.Meta["from_state"]), fmtAny(e.Meta["wake_ms"]))

	case e.Type == "thermal.snapshot":
		return fmt.Sprintf("idle=%ss", fmtAny(e.Meta["idle_s"]))

	case e.Type == "thermal.snapshot_failed":
		return fmt.Sprintf("error=%s attempt=%s", fmtAny(e.Meta["error"]), fmtAny(e.Meta["attempt"]))

	case e.Type == "thermal.force_pause":
		return fmt.Sprintf("failures=%s", fmtAny(e.Meta["consecutive_failures"]))

	case e.Type == "shell.session" || e.Type == "shell.web_session":
		return fmt.Sprintf("duration=%s", fmtDurationSecs(e.Meta["duration_s"]))

	case e.Type == "image.pulled":
		return fmt.Sprintf("ref=%s size=%sMB", fmtAny(e.Meta["ref"]), fmtAny(e.Meta["size_mb"]))

	case e.Type == "image.pull_failed":
		return fmt.Sprintf("ref=%s error=%s", fmtAny(e.Meta["ref"]), fmtAny(e.Meta["error"]))

	case e.Type == "publish.created":
		return fmt.Sprintf("port=%s alias=%s", fmtAny(e.Meta["port"]), fmtAny(e.Meta["alias"]))

	case e.Type == "auth.failed":
		return fmt.Sprintf("ip=%s", fmtAny(e.Meta["ip"]))

	case e.Type == "proxy.error":
		return fmt.Sprintf("alias=%s status=%s", fmtAny(e.Meta["alias"]), fmtAny(e.Meta["status"]))

	case e.Type == "volume.resized":
		return fmt.Sprintf("%sMB → %sMB", fmtAny(e.Meta["old_mb"]), fmtAny(e.Meta["new_mb"]))

	case e.Type == "daemon.started":
		return fmt.Sprintf("v%s recovered=%s", fmtAny(e.Meta["version"]), fmtAny(e.Meta["recovered_vms"]))

	case e.Type == "daemon.shutdown":
		return fmt.Sprintf("signal=%s", fmtAny(e.Meta["signal"]))
	}

	// Generic: dump first 3 meta keys
	if len(e.Meta) == 0 {
		return ""
	}
	var parts []string
	i := 0
	for k, v := range e.Meta {
		if i >= 3 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		i++
	}
	return strings.Join(parts, " ")
}

func fmtAny(v any) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%v", v)
}

func fmtDurationSecs(v any) string {
	switch n := v.(type) {
	case float64:
		return fmtHumanDuration(time.Duration(n) * time.Second)
	case int:
		return fmtHumanDuration(time.Duration(n) * time.Second)
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return fmtHumanDuration(time.Duration(f) * time.Second)
		}
	}
	return fmtAny(v)
}

func fmtHumanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

func fmtCount(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1_000_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
}


