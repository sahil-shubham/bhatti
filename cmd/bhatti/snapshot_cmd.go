package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// --- snapshot ---

var snapshotCmd = &cobra.Command{
	Use:   "snapshot <create|list|resume|delete>",
	Short: "Manage named VM snapshots",
	Long: `Snapshots capture the entire VM state: memory, CPU, disk. Resume
produces an exact continuation — processes running, files open.`,
	Example: `  bhatti snapshot create dev --name dev-ready
  bhatti snapshot resume dev-ready --name dev-2
  bhatti snapshot list`,
}

var snapshotCreateCmd = &cobra.Command{
	Use:               "create <sandbox-id|name>",
	Short:             "Checkpoint a running sandbox",
	Args:              exactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		name, _ := cmd.Flags().GetString("name")
		if name == "" {
			return fmt.Errorf("--name is required")
		}
		snapType, _ := cmd.Flags().GetString("type")

		var snap struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			SizeMB int    `json:"size_mb"`
		}
		body := map[string]any{"name": name}
		if snapType != "" {
			body["type"] = snapType
		}
		if err := apiJSON("POST", "/sandboxes/"+id+"/checkpoint", body, &snap); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(snap)
		} else {
			fmt.Printf("checkpoint %q created (%dMB)\n", snap.Name, snap.SizeMB)
		}
		return nil
	},
}

var snapshotListCmd = &cobra.Command{
	Use:   "list",
	Short: "List snapshots",
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		var snaps []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			SourceSandbox string `json:"source_sandbox"`
			SizeMB        int    `json:"size_mb"`
		}
		if err := apiJSON("GET", "/snapshots", nil, &snaps); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(snaps)
		} else {
			fmt.Printf("%-20s %-20s %-20s %-10s\n", "ID", "NAME", "SOURCE", "SIZE")
			for _, s := range snaps {
				fmt.Printf("%-20s %-20s %-20s %dMB\n", s.ID, s.Name, s.SourceSandbox, s.SizeMB)
			}
		}
		return nil
	},
}

var snapshotResumeCmd = &cobra.Command{
	Use:   "resume <snapshot-name>",
	Short: "Resume a sandbox from a snapshot",
	Args:  exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		name, _ := cmd.Flags().GetString("name")

		var sb struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			IP   string `json:"ip"`
		}
		body := map[string]any{}
		if name != "" {
			body["name"] = name
		}
		if err := apiJSON("POST", "/snapshots/"+args[0]+"/resume", body, &sb); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(sb)
		} else {
			fmt.Printf("%s\t%s\t%s\n", sb.ID, sb.Name, sb.IP)
		}
		return nil
	},
}

var snapshotDeleteCmd = &cobra.Command{
	Use:   "delete <snapshot-name>",
	Short: "Delete a snapshot",
	Args:  exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		if !confirmAction(cmd, fmt.Sprintf("Delete snapshot %q?", args[0])) {
			return nil
		}
		if err := apiJSON("DELETE", "/snapshots/"+args[0], nil, nil); err != nil {
			return err
		}
		fmt.Println("deleted")
		return nil
	},
}

func init() {
	snapshotCreateCmd.Flags().String("name", "", "Snapshot name (required)")
	snapshotCreateCmd.Flags().String("type", "", "Snapshot type: memory (default, RAM+disk) | filesystem (disk-only)")
	snapshotResumeCmd.Flags().String("name", "", "New sandbox name")

	snapshotCmd.AddCommand(snapshotCreateCmd)
	snapshotCmd.AddCommand(snapshotListCmd)
	snapshotCmd.AddCommand(snapshotResumeCmd)
	snapshotDeleteCmd.Flags().BoolP("yes", "y", false, "Skip confirmation")
	snapshotCmd.AddCommand(snapshotDeleteCmd)
}
