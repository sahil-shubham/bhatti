package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// --- volume ---

var volumeCmd = &cobra.Command{
	Use:   "volume <create|list|delete|resize>",
	Short: "Manage persistent volumes",
	Long: `Persistent volumes are ext4 filesystems that survive sandbox destruction.
Attach them with '--volume name:/mount' on create.`,
	Example: `  bhatti volume create --name workspace --size 5120
  bhatti create --name dev --volume workspace:/workspace
  bhatti volume resize workspace --size 10240
  bhatti volume list`,
}

var volumeCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a persistent volume",
	Example: `  bhatti volume create --name workspace --size 5120
  bhatti volume create --name data --size 20480`,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		name, _ := cmd.Flags().GetString("name")
		sizeMB, _ := cmd.Flags().GetInt("size")
		if name == "" || sizeMB <= 0 {
			return fmt.Errorf("--name and --size (> 0) required")
		}

		var vol struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			SizeMB int    `json:"size_mb"`
			Status string `json:"status"`
		}
		if err := apiJSON("POST", "/volumes", map[string]any{
			"name": name, "size_mb": sizeMB,
		}, &vol); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(vol)
		} else {
			fmt.Printf("%s\t%s\t%dMB\t%s\n", vol.ID, vol.Name, vol.SizeMB, vol.Status)
		}
		return nil
	},
}

var volumeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List persistent volumes",
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		var volumes []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			SizeMB int    `json:"size_mb"`
			Status string `json:"status"`
		}
		if err := apiJSON("GET", "/volumes", nil, &volumes); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(volumes)
		} else {
			fmt.Printf("%-20s %-20s %-10s %-10s\n", "ID", "NAME", "SIZE", "STATUS")
			for _, v := range volumes {
				fmt.Printf("%-20s %-20s %dMB\t%-10s\n", v.ID, v.Name, v.SizeMB, v.Status)
			}
		}
		return nil
	},
}

var volumeDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a persistent volume",
	Args:  exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		if !confirmAction(cmd, fmt.Sprintf("Delete volume %q?", args[0])) {
			return nil
		}
		if err := apiJSON("DELETE", "/volumes/"+args[0], nil, nil); err != nil {
			return err
		}
		fmt.Println("deleted")
		return nil
	},
}

var volumeResizeCmd = &cobra.Command{
	Use:   "resize <name>",
	Short: "Resize a persistent volume (grow only)",
	Args:  exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		sizeMB, _ := cmd.Flags().GetInt("size")
		if sizeMB <= 0 {
			return fmt.Errorf("--size (> 0) required")
		}

		if err := apiJSON("POST", "/volumes/"+args[0]+"/resize", map[string]any{
			"size_mb": sizeMB,
		}, nil); err != nil {
			return err
		}
		fmt.Printf("resized to %dMB\n", sizeMB)
		return nil
	},
}

func init() {
	volumeCreateCmd.Flags().String("name", "", "Volume name (required)")
	volumeCreateCmd.Flags().Int("size", 0, "Size in MB (required)")
	volumeResizeCmd.Flags().Int("size", 0, "New size in MB (must be larger)")

	volumeCmd.AddCommand(volumeCreateCmd)
	volumeCmd.AddCommand(volumeListCmd)
	volumeDeleteCmd.Flags().BoolP("yes", "y", false, "Skip confirmation")
	volumeCmd.AddCommand(volumeDeleteCmd)
	volumeCmd.AddCommand(volumeResizeCmd)
	volumeCloneCmd.Flags().String("name", "", "Name for the cloned volume (required)")
	volumeCmd.AddCommand(volumeCloneCmd)
	volumeCmd.AddCommand(volumeBackupCmd)
	volumeCmd.AddCommand(volumeBackupListCmd)
	volumeCmd.AddCommand(volumeRestoreCmd)
	volumeBackupDeleteCmd.Flags().BoolP("yes", "y", false, "Skip confirmation")
	volumeCmd.AddCommand(volumeBackupDeleteCmd)
}

// --- volume clone (B11) ---

var volumeCloneCmd = &cobra.Command{
	Use:   "clone <source-volume> --name <new-name>",
	Short: "Clone a volume (point-in-time copy)",
	Example: `  bhatti volume clone workspace --name workspace-backup`,
	Args: exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, _ := cmd.Flags().GetString("name")
		if name == "" {
			return fmt.Errorf("--name is required")
		}
		var result map[string]any
		if err := apiJSON("POST", "/volumes/"+args[0]+"/snapshot",
			map[string]string{"name": name}, &result); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(result)
		} else {
			fmt.Printf("Cloned %s → %s\n", args[0], name)
		}
		return nil
	},
}

var volumeBackupCmd = &cobra.Command{
	Use:   "backup <volume-name>",
	Short: "Backup a volume to S3-compatible storage",
	Args:  exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		var result struct {
			ID         string `json:"id"`
			VolumeName string `json:"volume_name"`
			SizeBytes  int64  `json:"size_bytes"`
			S3Key      string `json:"s3_key"`
			CreatedAt  string `json:"created_at"`
		}
		if err := apiJSON("POST", "/volumes/"+args[0]+"/backups", nil, &result); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(result)
		} else {
			fmt.Printf("backup %s created (%s, %d bytes)\n", result.ID, result.VolumeName, result.SizeBytes)
		}
		return nil
	},
}

var volumeBackupListCmd = &cobra.Command{
	Use:   "backup-list <volume-name>",
	Short: "List backups for a volume",
	Args:  exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		var backups []struct {
			ID        string `json:"id"`
			SizeBytes int64  `json:"size_bytes"`
			CreatedAt string `json:"created_at"`
		}
		if err := apiJSON("GET", "/volumes/"+args[0]+"/backups", nil, &backups); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(backups)
		} else {
			if len(backups) == 0 {
				fmt.Println("no backups")
				return nil
			}
			fmt.Printf("%-20s %-12s %s\n", "ID", "SIZE", "CREATED")
			for _, b := range backups {
				size := fmt.Sprintf("%d MB", b.SizeBytes/1024/1024)
				fmt.Printf("%-20s %-12s %s\n", b.ID, size, b.CreatedAt)
			}
		}
		return nil
	},
}

var volumeRestoreCmd = &cobra.Command{
	Use:   "restore <volume-name> --backup-id <id>",
	Short: "Restore a volume from a backup",
	Args:  exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		backupID, _ := cmd.Flags().GetString("backup-id")
		if backupID == "" {
			return fmt.Errorf("--backup-id required")
		}

		var result struct {
			Status   string `json:"status"`
			BackupID string `json:"backup_id"`
		}
		if err := apiJSON("POST", "/volumes/"+args[0]+"/backups/restore", map[string]any{
			"backup_id": backupID,
		}, &result); err != nil {
			return err
		}
		fmt.Printf("volume restored from backup %s\n", backupID)
		return nil
	},
}

var volumeBackupDeleteCmd = &cobra.Command{
	Use:   "backup-delete <volume-name> <backup-id>",
	Short: "Delete a volume backup",
	Args:  exactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		if !confirmAction(cmd, fmt.Sprintf("Delete backup %s?", args[1])) {
			return nil
		}
		if err := apiJSON("DELETE", "/volumes/"+args[0]+"/backups/"+args[1], nil, nil); err != nil {
			return err
		}
		fmt.Println("deleted")
		return nil
	},
}

func init() {
	volumeRestoreCmd.Flags().String("backup-id", "", "Backup ID to restore from (required)")
}
