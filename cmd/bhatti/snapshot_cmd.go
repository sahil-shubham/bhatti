package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// --- snapshot ---

var snapshotCmd = &cobra.Command{
	Use:   "snapshot <create|list|resume|delete|export|import>",
	Short: "Manage named VM snapshots",
	Long: `Snapshots capture the entire VM state: memory, CPU, disk. Resume
produces an exact continuation — processes running, files open. Export and
import move a snapshot to another host of the same OS and architecture.`,
	Example: `  bhatti snapshot create dev --name dev-ready
  bhatti snapshot resume dev-ready --name dev-2
  bhatti snapshot list
  bhatti snapshot export dev-ready -o dev-ready.tar.zst`,
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
			return errAborted
		}
		if err := apiJSON("DELETE", "/snapshots/"+args[0], nil, nil); err != nil {
			return err
		}
		fmt.Println("deleted")
		return nil
	},
}

var snapshotExportCmd = &cobra.Command{
	Use:   "export <snapshot-name>",
	Short: "Write a snapshot to an archive another host can import",
	Long: `Export a named snapshot as one archive (tar + zstd): its memory, CPU and
device state, its disks, and its manifest (size, volumes, network policy).
'bhatti snapshot import' takes it in on another host of the same OS and
architecture. Secrets and secret grants are not exported.

The root disk holds only what the sandbox changed; the rest comes from its base
image, which the archive names by content. The importing host must have that
image (any copy with the same content) unless the archive carries it:
--include-base.`,
	Example: `  bhatti snapshot export dev-ready -o dev-ready.tar.zst
  bhatti snapshot export dev-ready --include-base | ssh other-host bhatti snapshot import -`,
	Args: exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		out, _ := cmd.Flags().GetString("output")
		includeBase, _ := cmd.Flags().GetBool("include-base")
		toStdout := out == "" || out == "-"
		if toStdout && term.IsTerminal(int(os.Stdout.Fd())) {
			return fmt.Errorf("refusing to write a snapshot archive to a terminal: pass -o <file> or redirect stdout")
		}
		n, err := exportSnapshot(args[0], out, includeBase)
		if err != nil {
			return err
		}
		if toStdout {
			out = "stdout"
		}
		fmt.Fprintf(os.Stderr, "exported snapshot %q to %s (%dMB)\n", args[0], out, n>>20)
		return nil
	},
}

// exportSnapshot downloads the archive of snapshot name to out (stdout when
// "" or "-"). A file only appears under its name once the whole archive is
// in: an export that breaks off leaves nothing that could pass for one.
func exportSnapshot(name, out string, includeBase bool) (int64, error) {
	path := "/snapshots/" + url.PathEscape(name) + "/export"
	if includeBase {
		path += "?include_base=true"
	}
	req, err := http.NewRequest("GET", apiURL+path, nil)
	if err != nil {
		return 0, err
	}
	if apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+apiToken)
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, responseError(resp)
	}
	dst := io.Writer(os.Stdout)
	var f *os.File
	if out != "" && out != "-" {
		if f, err = os.Create(out + ".partial"); err != nil {
			return 0, err
		}
		dst = f
	}
	n, err := io.Copy(dst, resp.Body)
	if f != nil {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(f.Name(), out)
		}
		if err != nil {
			os.Remove(f.Name())
		}
	}
	if err != nil {
		return n, fmt.Errorf("export of snapshot %q broke off after %d bytes: %w", name, n, err)
	}
	return n, nil
}

var snapshotImportCmd = &cobra.Command{
	Use:   "import <archive|->",
	Short: "Register a snapshot exported on another host",
	Long: `Import a snapshot archive written by 'bhatti snapshot export' ('-' reads
stdin). The import is refused unless this host can run the snapshot: the same
OS and architecture, a CPU with every feature the guest uses, and the root
disk's base image (already here, found by content, or in the archive). Resume
it with 'bhatti snapshot resume'.`,
	Example: `  bhatti snapshot import dev-ready.tar.zst
  bhatti snapshot import dev-ready.tar.zst --name dev-ready-2
  bhatti snapshot resume dev-ready --name dev`,
	Args: exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		name, _ := cmd.Flags().GetString("name")
		snap, err := importSnapshot(args[0], name)
		if err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(snap)
		} else {
			fmt.Printf("snapshot %q imported (%dMB)\n", snap.Name, snap.SizeMB)
		}
		return nil
	},
}

type importedSnapshot struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	SizeMB int    `json:"size_mb"`
}

// importSnapshot uploads the archive at path ("-" = stdin) to register it as
// snapshot name, or under the name it was exported with when name is "".
func importSnapshot(path, name string) (importedSnapshot, error) {
	var snap importedSnapshot
	body := io.Reader(os.Stdin)
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return snap, err
		}
		defer f.Close()
		body = f
	}
	query := ""
	if name != "" {
		query = "?name=" + url.QueryEscape(name)
	}
	req, err := http.NewRequest("POST", apiURL+"/snapshots/import"+query, body)
	if err != nil {
		return snap, err
	}
	req.Header.Set("Content-Type", "application/zstd")
	if apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+apiToken)
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return snap, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return snap, responseError(resp)
	}
	return snap, json.NewDecoder(resp.Body).Decode(&snap)
}

// responseError is the API's {"error": ...} reply as an error.
func responseError(resp *http.Response) error {
	var body struct {
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	return fmt.Errorf("%s: %s", resp.Status, body.Error)
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
	snapshotExportCmd.Flags().StringP("output", "o", "", "Archive file to write (default: stdout)")
	snapshotExportCmd.Flags().Bool("include-base", false, "Carry the root disk's base image, for a host that doesn't have it")
	snapshotCmd.AddCommand(snapshotExportCmd)
	snapshotImportCmd.Flags().String("name", "", "Snapshot name (default: the name it was exported with)")
	snapshotCmd.AddCommand(snapshotImportCmd)
}
