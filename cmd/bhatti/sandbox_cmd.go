package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// parseLabelFlag turns []string{"k=v", "k2=v2"} into a map. Splits on
// the first `=` so values can contain further `=` chars. Returns an
// error on the first malformed entry. Empty input returns nil; callers
// guard on `len(labels) > 0` before populating the request body.
func parseLabelFlag(raw []string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(raw))
	for _, lf := range raw {
		eq := strings.IndexByte(lf, '=')
		if eq < 0 {
			return nil, fmt.Errorf("invalid --label %q: expected key=value", lf)
		}
		key := lf[:eq]
		if key == "" {
			return nil, fmt.Errorf("invalid --label %q: empty key", lf)
		}
		out[key] = lf[eq+1:]
	}
	return out, nil
}

// formatLabels renders a labels map as "k=v,k2=v2" with deterministic
// key ordering (alphabetical) for tabular ls -o wide output. Returns
// "-" for empty/nil so the column doesn't collapse.
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(labels))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ",")
}

var createCmd = &cobra.Command{
	Use:   "create [flags]",
	Short: "Create a new sandbox",
	Long: `Create a new sandbox VM. Each sandbox is an isolated Linux environment
with its own kernel, filesystem, and network.`,
	Example: `  # Basic sandbox
  bhatti create --name dev

  # Custom resources
  bhatti create --name ml --cpus 4 --memory 4096

  # With environment variables and init script
  bhatti create --name api --env API_KEY=sk-abc --init "npm install"

  # From a custom image
  bhatti create --name py --image python-3.12

  # With a persistent volume
  bhatti create --name work --volume workspace:/workspace

  # Autonomous agent (stays hot, never paused)
  bhatti create --name agent --init "hermes gateway" --keep-hot`,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		name, _ := cmd.Flags().GetString("name")
		image, _ := cmd.Flags().GetString("image")
		cpus, _ := cmd.Flags().GetFloat64("cpus")
		memory, _ := cmd.Flags().GetInt("memory")
		diskSize, _ := cmd.Flags().GetInt("disk-size")
		env, _ := cmd.Flags().GetString("env")
		initScript, _ := cmd.Flags().GetString("init")
		keepHot, _ := cmd.Flags().GetBool("keep-hot")
		hugepages, _ := cmd.Flags().GetBool("hugepages")
		volFlags, _ := cmd.Flags().GetStringSlice("volume")
		secretFlags, _ := cmd.Flags().GetStringSlice("secret")
		fileFlags, _ := cmd.Flags().GetStringSlice("file")
		labelFlags, _ := cmd.Flags().GetStringSlice("label")

		tmpl, _ := cmd.Flags().GetString("template")

		envMap := parseEnvFlag(env)
		req := map[string]any{
			"name": name, "cpus": cpus,
		}
		if tmpl != "" {
			req["template_id"] = tmpl
		}
		if memory > 0 {
			req["memory_mb"] = memory
		}
		if image != "" {
			req["image"] = image
		}
		if diskSize > 0 {
			req["disk_size_mb"] = diskSize
		}
		if len(envMap) > 0 {
			req["env"] = envMap
		}
		if initScript != "" {
			req["init"] = initScript
		}
		if keepHot {
			req["keep_hot"] = true
		}
		if hugepages {
			req["hugepages"] = true
		}

		// Parse --label flags: key=value pairs (repeatable)
		if len(labelFlags) > 0 {
			labels, err := parseLabelFlag(labelFlags)
			if err != nil {
				return err
			}
			req["labels"] = labels
		}

		// Parse --secret flags
		if len(secretFlags) > 0 {
			req["secrets"] = secretFlags
		}

		// Parse --file flags: local_path:guest_path
		if len(fileFlags) > 0 {
			var files []map[string]string
			for _, ff := range fileFlags {
				parts := strings.SplitN(ff, ":", 2)
				if len(parts) != 2 {
					return fmt.Errorf("invalid --file format %q (expected local_path:guest_path)", ff)
				}
				data, err := os.ReadFile(parts[0])
				if err != nil {
					return fmt.Errorf("read file %s: %w", parts[0], err)
				}
				files = append(files, map[string]string{
					"guest_path": parts[1],
					"content":    base64Encode(data),
				})
			}
			req["files"] = files
		}

		// Parse --volume flags: name:mount[:ro]
		if len(volFlags) > 0 {
			var pvols []map[string]any
			for _, vf := range volFlags {
				parts := strings.SplitN(vf, ":", 3)
				if len(parts) < 2 {
					return fmt.Errorf("invalid --volume format %q (expected name:mount[:ro])", vf)
				}
				pv := map[string]any{
					"name":        parts[0],
					"mount":       parts[1],
					"auto_create": false,
					"read_only":   false,
				}
				if len(parts) == 3 && parts[2] == "ro" {
					pv["read_only"] = true
				}
				pvols = append(pvols, pv)
			}
			req["persistent_volumes"] = pvols
		}

		var sb map[string]any
		resp, err := apiRequest("POST", "/sandboxes", req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		checkServerVersion(resp)
		if resp.StatusCode >= 400 {
			var errBody struct{ Error string `json:"error"` }
			json.NewDecoder(resp.Body).Decode(&errBody)
			return fmt.Errorf("%s: %s", resp.Status, errBody.Error)
		}
		json.NewDecoder(resp.Body).Decode(&sb)

		sbName, _ := sb["name"].(string)
		addToCompletionCache(sbName)

		existing := resp.Header.Get("X-Bhatti-Existing") == "true"

		if isJSON(cmd) {
			outputJSON(sb)
		} else if existing {
			fmt.Printf("sandbox/%s unchanged (already exists)\n", sbName)
		} else {
			// B1: verbose create output
			cpuVal, _ := sb["cpus"].(float64)
			if cpuVal == 0 {
				cpuVal = 1
			}
			memVal, _ := sb["memory_mb"].(float64)
			if memVal == 0 {
				memVal = 1024
			}
			diskVal, _ := sb["disk_size_mb"].(float64)
			ipVal, _ := sb["ip"].(string)

			// Only show disk in summary if user explicitly set it
			if diskVal > 0 {
				fmt.Printf("sandbox/%s created (%g vCPU, %.0f MB, %.0f MB disk)\n",
					sbName, cpuVal, memVal, diskVal)
			} else {
				fmt.Printf("sandbox/%s created (%g vCPU, %.0f MB)\n",
					sbName, cpuVal, memVal)
			}
			if ipVal != "" {
				fmt.Printf("  IP:    %s\n", ipVal)
			}
			fmt.Printf("  Shell: bhatti shell %s\n", sbName)
		}
		return nil
	},
}

func init() {
	createCmd.Flags().String("name", "", "Sandbox name")
	createCmd.Flags().String("image", "", "Rootfs image name")
	createCmd.Flags().Float64("cpus", 1, "Number of vCPUs")
	createCmd.Flags().Int("memory", 0, "Memory in MB (0 = server default: 1024)")
	createCmd.Flags().Int("disk-size", 0, "Rootfs disk size in MB (0 = use image size)")
	createCmd.Flags().String("env", "", "Environment variables (K=V,K=V)")
	createCmd.Flags().String("init", "", "Init script")
	createCmd.Flags().Bool("keep-hot", false, "Prevent thermal transitions (for autonomous agents)")
	createCmd.Flags().Bool("hugepages", false, "Use 2MB hugepages (faster boot, no diff snapshots)")
	createCmd.Flags().String("template", "", "Template name or ID")
	createCmd.Flags().StringSlice("volume", nil, "Persistent volume (name:mount[:ro])")
	createCmd.Flags().StringSlice("secret", nil, "Secret name from store (repeatable)")
	createCmd.Flags().StringSlice("file", nil, "Inject file (local_path:guest_path, repeatable)")
	createCmd.Flags().StringSlice("label", nil, "Set label key=value (repeatable)")

	editCmd.Flags().Bool("keep-hot", false, "Prevent thermal transitions (for autonomous agents)")
	editCmd.Flags().Bool("allow-cold", false, "Re-enable thermal transitions")
	editCmd.Flags().String("name", "", "Rename sandbox")
	editCmd.Flags().StringSlice("label", nil, "Set or update label key=value (repeatable)")
	editCmd.Flags().StringSlice("label-delete", nil, "Remove label by key (repeatable)")

	listCmd.Flags().StringSlice("label", nil, "Filter by label key=value (AND across multiple, repeatable)")

	startCmd.Flags().Bool("force", false, "Force start (retry after failed restore)")
}

// --- edit ---

var editCmd = &cobra.Command{
	Use:   "edit <sandbox> [flags]",
	Short: "Update sandbox settings",
	Long: `Update mutable settings on an existing sandbox. Supports renaming
and toggling keep_hot to control thermal management.`,
	Example: `  # Prevent a sandbox from being paused/snapshotted
  bhatti edit my-agent --keep-hot

  # Re-enable thermal transitions
  bhatti edit my-agent --allow-cold

  # Rename a sandbox
  bhatti edit dev --name dev-old`,
	Args:              exactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := resolveID(args[0])
		if err != nil {
			return err
		}

		req := map[string]any{}
		keepHot, _ := cmd.Flags().GetBool("keep-hot")
		allowCold, _ := cmd.Flags().GetBool("allow-cold")
		newName, _ := cmd.Flags().GetString("name")
		if keepHot && allowCold {
			return fmt.Errorf("cannot use --keep-hot and --allow-cold together")
		}
		if keepHot {
			req["keep_hot"] = true
		}
		if allowCold {
			req["keep_hot"] = false
		}
		if newName != "" {
			req["name"] = newName
		}

		labelFlags, _ := cmd.Flags().GetStringSlice("label")
		labelDeleteFlags, _ := cmd.Flags().GetStringSlice("label-delete")
		if len(labelFlags) > 0 {
			add, err := parseLabelFlag(labelFlags)
			if err != nil {
				return err
			}
			req["labels_add"] = add
		}
		if len(labelDeleteFlags) > 0 {
			req["labels_remove"] = labelDeleteFlags
		}

		if len(req) == 0 {
			return fmt.Errorf("nothing to update — use --name, --keep-hot, --allow-cold, --label, or --label-delete")
		}

		var sb map[string]any
		if err := apiJSON("PATCH", "/sandboxes/"+id, req, &sb); err != nil {
			return err
		}

		// Keep the local completion cache in sync. The cache is also
		// rebuilt on every `bhatti ls`, so this is just for users who
		// rename and immediately tab-complete.
		if newName != "" && newName != args[0] {
			removeFromCompletionCache(args[0])
			addToCompletionCache(newName)
		}

		if isJSON(cmd) {
			outputJSON(sb)
		} else {
			fmt.Printf("Updated %s\n", args[0])
			if newName != "" && newName != args[0] {
				fmt.Printf("  name:     %s\n", newName)
				// The in-guest hostname is set at create time via the
				// config drive and is not changed by rename. Heads off
				// the surprise of seeing the old name in shell prompts.
				fmt.Println("  Note: in-guest hostname unchanged (set at create time)")
			}
			if keepHot {
				fmt.Println("  keep_hot: true (thermal transitions disabled)")
			}
			if allowCold {
				fmt.Println("  keep_hot: false (thermal transitions re-enabled)")
			}
		}
		return nil
	},
}

// --- stop ---

var stopCmd = &cobra.Command{
	Use:   "stop <sandbox>",
	Short: "Snapshot and stop a sandbox",
	Long: `Pause the sandbox and save a snapshot to disk. Resume later with
'bhatti start'. Stopped sandboxes use zero CPU and memory.`,
	Example: `  bhatti stop dev
  bhatti start dev     # resume later`,
	Args:              exactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()
		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		var sb map[string]any
		if err := apiJSON("POST", "/sandboxes/"+id+"/stop", nil, &sb); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(sb)
		} else {
			fmt.Printf("sandbox/%s stopped\n", args[0])
		}
		return nil
	},
}

// --- start ---

var startCmd = &cobra.Command{
	Use:   "start <sandbox>",
	Short: "Resume a stopped sandbox",
	Long: `Resume a sandbox from its snapshot. Continues exactly where it left off.
Use --force to retry after a failed restore.`,
	Example: `  bhatti start dev
  bhatti start dev --force`,
	Args:              exactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()
		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		force, _ := cmd.Flags().GetBool("force")
		var body map[string]any
		if force {
			body = map[string]any{"force": true}
		}
		var sb map[string]any
		if err := apiJSON("POST", "/sandboxes/"+id+"/start", body, &sb); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(sb)
		} else {
			fmt.Printf("sandbox/%s started\n", args[0])
		}
		return nil
	},
}

// --- inspect ---

var inspectCmd = &cobra.Command{
	Use:     "inspect <sandbox>",
	Short:   "Show sandbox details",
	Aliases: []string{"info"},
	Example: `  bhatti inspect dev
  bhatti inspect dev --json`,
	Args:              exactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()
		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		var sb map[string]any
		if err := apiJSON("GET", "/sandboxes/"+id, nil, &sb); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(sb)
			return nil
		}

		// B4: kubectl-describe style output
		fmt.Printf("Name:       %s\n", sb["name"])
		fmt.Printf("ID:         %s\n", sb["id"])
		fmt.Printf("Status:     %s\n", sb["status"])
		if t, ok := sb["thermal"]; ok && t != nil && t != "" {
			fmt.Printf("Thermal:    %s\n", t)
		}
		if img, ok := sb["image"]; ok && img != nil && img != "" {
			fmt.Printf("Image:      %s\n", img)
		}
		fmt.Printf("Created:    %s\n", sb["created_at"])
		if stopped, ok := sb["stopped_at"]; ok && stopped != nil {
			fmt.Printf("Stopped:    %s\n", stopped)
		}
		if t, ok := sb["template_id"]; ok && t != nil && t != "" {
			fmt.Printf("Template:   %s\n", t)
		}

		fmt.Println()
		fmt.Println("Resources:")
		cpus, _ := sb["cpus"].(float64)
		if cpus == 0 {
			cpus = 1
		}
		fmt.Printf("  CPUs:     %g\n", cpus)
		memMB, _ := sb["memory_mb"].(float64)
		if memMB == 0 {
			memMB = 1024
		}
		fmt.Printf("  Memory:   %.0f MB\n", memMB)

		// Live disk usage from df (running VMs only)
		status, _ := sb["status"].(string)
		if status == "running" {
			var dfResult struct {
				Stdout string `json:"stdout"`
			}
			if err := apiJSON("POST", "/sandboxes/"+id+"/exec",
				map[string]any{"cmd": []string{"df", "-m", "/"}}, &dfResult); err == nil {
				lines := strings.Split(dfResult.Stdout, "\n")
				if len(lines) >= 2 {
					fields := strings.Fields(lines[1])
					if len(fields) >= 4 {
						var totalMB, usedMB, freeMB int
						fmt.Sscanf(fields[1], "%d", &totalMB)
						fmt.Sscanf(fields[2], "%d", &usedMB)
						fmt.Sscanf(fields[3], "%d", &freeMB)
						fmt.Printf("  Disk:     %d MB (%d MB used, %d MB free)\n", totalMB, usedMB, freeMB)
					}
				}
			}
		} else {
			diskMB, _ := sb["disk_size_mb"].(float64)
			if diskMB > 0 {
				fmt.Printf("  Disk:     %.0f MB\n", diskMB)
			} else {
				fmt.Printf("  Disk:     — (stopped)\n")
			}
		}

		fmt.Println()
		fmt.Println("Network:")
		fmt.Printf("  IP:       %s\n", sb["ip"])

		if kh, ok := sb["keep_hot"]; ok && kh == true {
			fmt.Println()
			fmt.Println("  keep_hot: true")
		}
		return nil
	},
}

// --- list ---

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List sandboxes",
	Example: `  bhatti list
  bhatti ls            # alias
  bhatti ls --json
  bhatti ls -o wide    # show resources and image`,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		var sandboxes []struct {
			ID         string            `json:"id"`
			Name       string            `json:"name"`
			Status     string            `json:"status"`
			Thermal    string            `json:"thermal"`
			IP         string            `json:"ip"`
			URLs       []string          `json:"urls"`
			CPUs       float64           `json:"cpus"`
			MemoryMB   float64           `json:"memory_mb"`
			DiskSizeMB float64           `json:"disk_size_mb"`
			Image      string            `json:"image"`
			Labels     map[string]string `json:"labels,omitempty"`
		}
		// Build query string with optional --label filters.
		path := "/sandboxes"
		labelFlags, _ := cmd.Flags().GetStringSlice("label")
		if len(labelFlags) > 0 {
			q := url.Values{}
			for _, lf := range labelFlags {
				// Pass through as-is; the server validates k=v shape and
				// returns 400 on invalid input. The client doesn't try to
				// pre-validate so the user gets one consistent error path.
				q.Add("label", lf)
			}
			path += "?" + q.Encode()
		}
		if err := apiJSON("GET", path, nil, &sandboxes); err != nil {
			return err
		}

		// Update completion cache (best-effort, never errors)
		var names []string
		for _, sb := range sandboxes {
			names = append(names, sb.Name)
		}
		os.WriteFile(completionCachePath(), []byte(strings.Join(names, "\n")), 0600)

		output, _ := cmd.Flags().GetString("output")
		wide := output == "wide"

		if isJSON(cmd) {
			outputJSON(sandboxes)
		} else if wide {
			// B6: wide mode with resources and image
			fmt.Printf("%-20s %-10s %-8s %-16s %-6s %-8s %-8s %-12s %s\n",
				"NAME", "STATUS", "THERMAL", "IP", "CPUS", "MEMORY", "DISK", "IMAGE", "LABELS")
			for _, sb := range sandboxes {
				thermal := sb.Thermal
				if thermal == "" {
					thermal = "-"
				}
				cpus := sb.CPUs
				if cpus == 0 {
					cpus = 1
				}
				mem := sb.MemoryMB
				if mem == 0 {
					mem = 1024
				}
				img := sb.Image
				if img == "" {
					img = "minimal"
				}
				fmt.Printf("%-20s %-10s %-8s %-16s %-6g %-8.0f %-8.0f %-12s %s\n",
					sb.Name, sb.Status, thermal, sb.IP, cpus, mem, sb.DiskSizeMB, img, formatLabels(sb.Labels))
			}
		} else {
			// B6: clean default — no ID column
			hasURLs := false
			for _, sb := range sandboxes {
				if len(sb.URLs) > 0 {
					hasURLs = true
					break
				}
			}
			if hasURLs {
				fmt.Printf("%-20s %-10s %-8s %-16s %s\n", "NAME", "STATUS", "THERMAL", "IP", "URL")
			} else {
				fmt.Printf("%-20s %-10s %-8s %-16s\n", "NAME", "STATUS", "THERMAL", "IP")
			}
			for _, sb := range sandboxes {
				thermal := sb.Thermal
				if thermal == "" {
					thermal = "-"
				}
				if hasURLs {
					url := ""
					if len(sb.URLs) > 0 {
						url = sb.URLs[0]
					}
					fmt.Printf("%-20s %-10s %-8s %-16s %s\n", sb.Name, sb.Status, thermal, sb.IP, url)
				} else {
					fmt.Printf("%-20s %-10s %-8s %-16s\n", sb.Name, sb.Status, thermal, sb.IP)
				}
			}
		}
		return nil
	},
}

// --- destroy ---

var destroyCmd = &cobra.Command{
	Use:               "destroy <id|name>",
	Aliases:           []string{"rm"},
	Short:             "Destroy a sandbox",
	Long: `Permanently destroy a sandbox and all its data. This cannot be undone.
Persistent volumes are detached but not deleted.`,
	Example: `  bhatti destroy dev
  bhatti rm dev        # alias`,
	Args:              exactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		if !confirmAction(cmd, fmt.Sprintf("Destroy sandbox %q?", args[0])) {
			return nil
		}

		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		if err := apiJSON("DELETE", "/sandboxes/"+id, nil, nil); err != nil {
			return err
		}
		removeFromCompletionCache(args[0])
		if isJSON(cmd) {
			outputJSON(map[string]string{"status": "destroyed"})
		} else {
			fmt.Printf("sandbox/%s destroyed\n", args[0])
		}
		return nil
	},
}

