package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
)


// --- image ---

var imageCmd = &cobra.Command{
	Use:   "image <list|pull|save|delete>",
	Short: "Manage rootfs images",
	Long: `Images are ext4 filesystem snapshots used as sandbox root filesystems.
Pull public images from registries with 'image pull'.`,
	Example: `  # Pull a public image
  bhatti image pull python:3.12

  # Save a running sandbox as an image
  bhatti image save dev --name my-custom-env

  # List images
  bhatti image list`,
}

var imageListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available images",
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		var images []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Source string `json:"source"`
			SizeMB int    `json:"size_mb"`
			UserID string `json:"user_id"`
		}
		if err := apiJSON("GET", "/images", nil, &images); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(images)
		} else {
			fmt.Printf("%-20s %-20s %-30s %-10s %-10s\n", "ID", "NAME", "SOURCE", "SIZE", "SCOPE")
			for _, img := range images {
				scope := "admin"
				if img.UserID != "" {
					scope = "user"
				}
				fmt.Printf("%-20s %-20s %-30s %dMB\t%-10s\n",
					img.ID, img.Name, img.Source, img.SizeMB, scope)
			}
		}
		return nil
	},
}

var imageDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete an image",
	Args:  exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		if !confirmAction(cmd, fmt.Sprintf("Delete image %q?", args[0])) {
			return nil
		}
		if err := apiJSON("DELETE", "/images/"+args[0], nil, nil); err != nil {
			return err
		}
		fmt.Println("deleted")
		return nil
	},
}

var imagePullCmd = &cobra.Command{
	Use:   "pull <ref>",
	Short: "Pull an OCI/Docker image from a public registry",
	Long: `Pull a public image from any OCI-compatible registry. The server pulls
the image and converts it to an ext4 rootfs for 'bhatti create --image'.`,
	Example: `  bhatti image pull python:3.12
  bhatti image pull ubuntu:24.04 --name ubuntu
  bhatti image pull node:22-slim --name node-22`,
	Args:  exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		ref := args[0]
		name, _ := cmd.Flags().GetString("name")
		auth, _ := cmd.Flags().GetString("auth")
		if name == "" {
			// Derive name from ref: "python:3.12" → "python-3.12"
			name = strings.NewReplacer("/", "-", ":", "-", ".", "-").Replace(ref)
			// Remove registry prefix for common cases
			if idx := strings.LastIndex(name, "-"); idx > 0 {
				parts := strings.Split(ref, "/")
				if len(parts) > 1 {
					name = strings.NewReplacer("/", "-", ":", "-").Replace(
						strings.Join(parts[len(parts)-1:], "-"))
				}
			}
		}

		body := map[string]any{"ref": ref, "name": name}
		if auth != "" {
			body["auth"] = auth
		}

		var result struct {
			TaskID string `json:"task_id"`
			Status string `json:"status"`
		}
		if err := apiJSON("POST", "/images/pull", body, &result); err != nil {
			return err
		}

		if isJSON(cmd) {
			outputJSON(result)
			return nil
		}

		fmt.Printf("pulling %s as %q (task: %s)\n", ref, name, result.TaskID)

		// B8: trap Ctrl+C — pull continues on server
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		defer signal.Stop(sigCh)

		// Poll for completion
		for {
			select {
			case <-sigCh:
				fmt.Fprintf(os.Stderr, "\nInterrupted. Pull continues on server.\n")
				fmt.Fprintf(os.Stderr, "  Check status: bhatti image list\n")
				return nil
			default:
			}
			time.Sleep(2 * time.Second)
			var task struct {
				Status   string `json:"status"`
				Progress string `json:"progress"`
				Error    string `json:"error"`
			}
			if err := apiJSON("GET", "/tasks/"+result.TaskID, nil, &task); err != nil {
				return err
			}
			switch task.Status {
			case "completed":
				fmt.Println("done")
				return nil
			case "failed":
				if strings.Contains(task.Error, "unauthorized") ||
					strings.Contains(task.Error, "UNAUTHORIZED") ||
					strings.Contains(task.Error, "403") {
					fmt.Fprintf(os.Stderr, "This image may require authentication.\n")
					fmt.Fprintf(os.Stderr, "Pull it with Docker locally, then import:\n")
					fmt.Fprintf(os.Stderr, "  docker pull %s\n", ref)
					fmt.Fprintf(os.Stderr, "  bhatti image import %s\n", ref)
					return fmt.Errorf("pull failed: authentication required")
				}
				return fmt.Errorf("pull failed: %s", task.Error)
			default:
				if task.Progress != "" {
					fmt.Printf("\r  %s", task.Progress)
				}
			}
		}
	},
}

var imageSaveCmd = &cobra.Command{
	Use:               "save <sandbox-id|name>",
	Short:             "Save a sandbox's rootfs as an image",
	Example: `  bhatti image save dev --name my-custom-env`,
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

		var img struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			SizeMB int    `json:"size_mb"`
		}
		if err := apiJSON("POST", "/sandboxes/"+id+"/save-image",
			map[string]any{"name": name}, &img); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(img)
		} else {
			fmt.Printf("saved %q (%dMB)\n", img.Name, img.SizeMB)
		}
		return nil
	},
}

var imageImportCmd = &cobra.Command{
	Use:   "import <docker-ref>",
	Short: "Import a local Docker image as a bhatti rootfs",
	Long: `Import an image that exists in your local Docker daemon. The CLI runs
'docker save' on your machine and streams the result to the bhatti server,
which converts it to an ext4 rootfs for use with 'bhatti create --image'.

The image name is derived from the ref automatically. Use --name to override.
Docker must be installed locally.

For private registries, pull with Docker first (which handles auth):
  docker pull ghcr.io/org/private:latest
  bhatti image import ghcr.io/org/private:latest

For raw tarballs (no Docker needed), use --tar:
  bhatti image import --tar /path/to/image.tar --name my-image`,
	Example: `  # Import from local Docker (name derived from ref)
  bhatti image import python:3.12

  # Private image (pull with Docker first)
  docker pull ghcr.io/org/private:latest
  bhatti image import ghcr.io/org/private:latest

  # Locally built image
  docker build -t my-env .
  bhatti image import my-env

  # Custom name
  bhatti image import python:3.12 --name py312

  # From a raw tarball (--name required)
  bhatti image import --tar /tmp/image.tar --name from-tar`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		name, _ := cmd.Flags().GetString("name")
		tarPath, _ := cmd.Flags().GetString("tar")

		var body io.Reader

		if tarPath != "" {
			if name == "" {
				return fmt.Errorf("--name is required when using --tar")
			}
			f, err := os.Open(tarPath)
			if err != nil {
				return fmt.Errorf("open tarball: %w", err)
			}
			defer f.Close()
			body = f
			fmt.Fprintf(os.Stderr, "importing %q from tarball...\n", name)
		} else {
			if len(args) == 0 {
				return fmt.Errorf("docker image ref required (or use --tar)")
			}
			ref := args[0]

			if name == "" {
				name = deriveImageName(ref)
			}

			if _, err := exec.LookPath("docker"); err != nil {
				return fmt.Errorf("docker not found — install Docker or use --tar with a tarball")
			}

			check := exec.Command("docker", "image", "inspect", ref)
			check.Stdout = nil
			check.Stderr = nil
			if err := check.Run(); err != nil {
				return fmt.Errorf("image %q not found in Docker — run 'docker pull %s' first", ref, ref)
			}

			save := exec.Command("docker", "save", ref)
			stdout, err := save.StdoutPipe()
			if err != nil {
				return fmt.Errorf("docker save pipe: %w", err)
			}
			if err := save.Start(); err != nil {
				return fmt.Errorf("docker save start: %w", err)
			}
			defer save.Wait()
			body = stdout
			fmt.Fprintf(os.Stderr, "importing %q from local Docker...\n", ref)
		}

		req, err := http.NewRequest("POST",
			apiURL+"/images/import?name="+url.QueryEscape(name),
			body)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-tar")
		if apiToken != "" {
			req.Header.Set("Authorization", "Bearer "+apiToken)
		}

		resp, err := httpClient().Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			var errBody struct{ Error string `json:"error"` }
			json.NewDecoder(resp.Body).Decode(&errBody)
			return fmt.Errorf("%s: %s", resp.Status, errBody.Error)
		}

		var result struct {
			Name   string `json:"name"`
			SizeMB int    `json:"size_mb"`
		}
		json.NewDecoder(resp.Body).Decode(&result)

		if isJSON(cmd) {
			outputJSON(result)
		} else {
			fmt.Printf("imported %q (%dMB)\n", result.Name, result.SizeMB)
		}
		return nil
	},
}

// deriveImageName extracts a short image name from a Docker/OCI ref.
//
//	"python:3.12"                   → "python-3.12"
//	"ghcr.io/org/private:latest"    → "private-latest"
//	"my-env"                        → "my-env"
//	"docker.io/library/python:3.12" → "python-3.12"
func deriveImageName(ref string) string {
	parts := strings.Split(ref, "/")
	base := parts[len(parts)-1]
	return strings.ReplaceAll(base, ":", "-")
}

func init() {
	imagePullCmd.Flags().String("name", "", "Image name (default: derived from ref)")
	imagePullCmd.Flags().String("auth", "", "Registry auth (user:token)")
	imageSaveCmd.Flags().String("name", "", "Image name (required)")
	imageImportCmd.Flags().String("name", "", "Image name (default: derived from ref)")
	imageImportCmd.Flags().String("tar", "", "Import from tarball path instead of Docker")

	imageCmd.AddCommand(imageListCmd)
	imageDeleteCmd.Flags().BoolP("yes", "y", false, "Skip confirmation")
	imageCmd.AddCommand(imageDeleteCmd)
	imageCmd.AddCommand(imagePullCmd)
	imageCmd.AddCommand(imageImportCmd)
	imageCmd.AddCommand(imageSaveCmd)
	imageShareCmd.Flags().StringSlice("user", nil, "User name(s) to share with")
	imageShareCmd.Flags().Bool("list", false, "List current shares")
	imageCmd.AddCommand(imageShareCmd)
	imageUnshareCmd.Flags().StringSlice("user", nil, "User name(s) to unshare from")
	imageCmd.AddCommand(imageUnshareCmd)
}

var imageShareCmd = &cobra.Command{
	Use:   "share <image-name>",
	Short: "Share an image with other users (requires DB access)",
	Example: `  sudo bhatti image share spc-golden --user kowshik --user sumo
  sudo bhatti image share spc-golden --list`,
	Args: exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		st := openLocalStore()
		defer st.Close()

		imageName := args[0]
		listShares, _ := cmd.Flags().GetBool("list")
		users, _ := cmd.Flags().GetStringSlice("user")

		img, err := st.GetImageByName(imageName)
		if err != nil {
			return err
		}

		if listShares {
			shares, _ := st.ListImageShares(img.ID)
			if len(shares) == 0 {
				fmt.Printf("%s: not shared\n", imageName)
			} else {
				fmt.Printf("%s shared with: %s\n", imageName, strings.Join(shares, ", "))
			}
			return nil
		}

		if len(users) == 0 {
			return fmt.Errorf("--user required (or use --list)")
		}

		for _, userName := range users {
			user, err := st.GetUserByName(userName)
			if err != nil {
				return err
			}
			if err := st.ShareImage(img.ID, user.ID); err != nil {
				return fmt.Errorf("share with %q: %w", userName, err)
			}
		}
		fmt.Printf("shared %q with: %s\n", imageName, strings.Join(users, ", "))
		return nil
	},
}

var imageUnshareCmd = &cobra.Command{
	Use:   "unshare <image-name>",
	Short: "Revoke image access from users (requires DB access)",
	Example: `  sudo bhatti image unshare spc-golden --user sumo`,
	Args: exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		st := openLocalStore()
		defer st.Close()

		imageName := args[0]
		users, _ := cmd.Flags().GetStringSlice("user")

		if len(users) == 0 {
			return fmt.Errorf("--user required")
		}

		img, err := st.GetImageByName(imageName)
		if err != nil {
			return err
		}

		for _, userName := range users {
			user, err := st.GetUserByName(userName)
			if err != nil {
				return err
			}
			st.UnshareImage(img.ID, user.ID)
		}
		fmt.Printf("unshared %q from: %s\n", imageName, strings.Join(users, ", "))
		return nil
	},
}

