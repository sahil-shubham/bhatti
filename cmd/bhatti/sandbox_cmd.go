package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

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
		volFlags, _ := cmd.Flags().GetStringSlice("volume")

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

		var sb struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			IP   string `json:"ip"`
		}
		if err := apiJSON("POST", "/sandboxes", req, &sb); err != nil {
			return err
		}
		addToCompletionCache(sb.Name)
		if isJSON(cmd) {
			outputJSON(sb)
		} else {
			fmt.Printf("%s\t%s\t%s\n", sb.ID, sb.Name, sb.IP)
		}
		return nil
	},
}

func init() {
	createCmd.Flags().String("name", "", "Sandbox name")
	createCmd.Flags().String("image", "", "Rootfs image name")
	createCmd.Flags().Float64("cpus", 1, "Number of vCPUs")
	createCmd.Flags().Int("memory", 0, "Memory in MB (0 = server default: 2048)")
	createCmd.Flags().Int("disk-size", 0, "Rootfs disk size in MB (0 = use image size)")
	createCmd.Flags().String("env", "", "Environment variables (K=V,K=V)")
	createCmd.Flags().String("init", "", "Init script")
	createCmd.Flags().Bool("keep-hot", false, "Prevent thermal transitions (for autonomous agents)")
	createCmd.Flags().String("template", "", "Template name or ID")
	createCmd.Flags().StringSlice("volume", nil, "Persistent volume (name:mount[:ro])")

	editCmd.Flags().Bool("keep-hot", false, "Prevent thermal transitions (for autonomous agents)")
	editCmd.Flags().Bool("allow-cold", false, "Re-enable thermal transitions")
}

// --- edit ---

var editCmd = &cobra.Command{
	Use:   "edit <sandbox> [flags]",
	Short: "Update sandbox settings",
	Long: `Update mutable settings on an existing sandbox. Currently supports
toggling keep_hot to control thermal management.`,
	Example: `  # Prevent a sandbox from being paused/snapshotted
  bhatti edit my-agent --keep-hot

  # Re-enable thermal transitions
  bhatti edit my-agent --allow-cold`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := resolveID(args[0])
		if err != nil {
			return err
		}

		req := map[string]any{}
		keepHot, _ := cmd.Flags().GetBool("keep-hot")
		allowCold, _ := cmd.Flags().GetBool("allow-cold")
		if keepHot && allowCold {
			return fmt.Errorf("cannot use --keep-hot and --allow-cold together")
		}
		if keepHot {
			req["keep_hot"] = true
		}
		if allowCold {
			req["keep_hot"] = false
		}

		if len(req) == 0 {
			return fmt.Errorf("nothing to update — use --keep-hot or --allow-cold")
		}

		var sb map[string]any
		if err := apiJSON("PATCH", "/sandboxes/"+id, req, &sb); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(sb)
		} else {
			fmt.Printf("Updated %s\n", args[0])
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
	Args:              cobra.ExactArgs(1),
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
			fmt.Println("stopped")
		}
		return nil
	},
}

// --- start ---

var startCmd = &cobra.Command{
	Use:   "start <sandbox>",
	Short: "Resume a stopped sandbox",
	Long:  `Resume a sandbox from its snapshot. Continues exactly where it left off.`,
	Example: `  bhatti start dev`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()
		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		var sb map[string]any
		if err := apiJSON("POST", "/sandboxes/"+id+"/start", nil, &sb); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(sb)
		} else {
			fmt.Printf("started (%s)\n", sb["status"])
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
	Args:              cobra.ExactArgs(1),
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
		fmt.Printf("Name:       %s\n", sb["name"])
		fmt.Printf("ID:         %s\n", sb["id"])
		fmt.Printf("Status:     %s\n", sb["status"])
		fmt.Printf("IP:         %s\n", sb["ip"])
		fmt.Printf("Created:    %s\n", sb["created_at"])
		if t, ok := sb["template_id"]; ok && t != nil && t != "" {
			fmt.Printf("Template:   %s\n", t)
		}
		if stopped, ok := sb["stopped_at"]; ok && stopped != nil {
			fmt.Printf("Stopped:    %s\n", stopped)
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
  bhatti ls --json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		var sandboxes []struct {
			ID      string   `json:"id"`
			Name    string   `json:"name"`
			Status  string   `json:"status"`
			Thermal string   `json:"thermal"`
			IP      string   `json:"ip"`
			URLs    []string `json:"urls"`
		}
		if err := apiJSON("GET", "/sandboxes", nil, &sandboxes); err != nil {
			return err
		}

		// Update completion cache (best-effort, never errors)
		var names []string
		for _, sb := range sandboxes {
			names = append(names, sb.Name)
		}
		os.WriteFile(completionCachePath(), []byte(strings.Join(names, "\n")), 0600)

		if isJSON(cmd) {
			outputJSON(sandboxes)
		} else {
			// Show URL column only if any sandbox has published URLs
			hasURLs := false
			for _, sb := range sandboxes {
				if len(sb.URLs) > 0 {
					hasURLs = true
					break
				}
			}

			if hasURLs {
				fmt.Printf("%-20s %-20s %-10s %-8s %-16s %s\n", "ID", "NAME", "STATUS", "THERMAL", "IP", "URL")
			} else {
				fmt.Printf("%-20s %-20s %-10s %-8s %-16s\n", "ID", "NAME", "STATUS", "THERMAL", "IP")
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
					fmt.Printf("%-20s %-20s %-10s %-8s %-16s %s\n", sb.ID, sb.Name, sb.Status, thermal, sb.IP, url)
				} else {
					fmt.Printf("%-20s %-20s %-10s %-8s %-16s\n", sb.ID, sb.Name, sb.Status, thermal, sb.IP)
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
	Args:              cobra.ExactArgs(1),
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
			fmt.Println("destroyed")
		}
		return nil
	},
}

