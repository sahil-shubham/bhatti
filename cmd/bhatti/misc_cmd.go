package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sahil-shubham/bhatti/pkg"
	"github.com/spf13/cobra"
)

// --- update ---

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update bhatti to the latest version",
	Long: `Update bhatti to the latest release. On a server, updates all
components (bhatti, Firecracker, lohar, kernel, rootfs). On a CLI-only
machine, updates just the binary.

Use --cli-only to update only the binary on a server.
Use --tiers to install additional rootfs tiers during the update.`,
	Example: `  bhatti update                   # auto-detect CLI vs server
  sudo bhatti update               # server update (requires root)
  sudo bhatti update --tiers all   # server update + pull all tiers
  bhatti update --cli-only         # binary only, even on a server`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cliOnly, _ := cmd.Flags().GetBool("cli-only")
		tiers, _ := cmd.Flags().GetString("tiers")

		// Detect if this is a server by checking for config file
		isServer := false
		for _, p := range []string{"/etc/bhatti/config.yaml", "/var/lib/bhatti/config.yaml"} {
			if _, err := os.Stat(p); err == nil {
				isServer = true
				break
			}
		}

		// Fail fast: server update requires root, don't download the
		// install script just to fail inside it.
		if !cliOnly && isServer && os.Getuid() != 0 {
			return fmt.Errorf("server update requires root\n  Re-run with: sudo bhatti update")
		}

		fmt.Printf("Current version: %s\n", version)

		// Find a shell to run the installer
		shellBin := "/bin/sh"
		if _, err := os.Stat("/bin/bash"); err == nil {
			shellBin = "/bin/bash"
		}

		fmt.Println("Downloading latest version...")
		install := exec.Command(shellBin, "-c", "curl -fsSL bhatti.sh/install | bash")
		install.Stdout = os.Stdout
		install.Stderr = os.Stderr

		// Build env: let install.sh auto-detect server vs CLI.
		// Only force CLI mode if --cli-only is set.
		env := os.Environ()
		if cliOnly {
			env = append(env, "BHATTI_MODE=cli")
		}
		if tiers != "" {
			env = append(env, "BHATTI_TIERS="+tiers)
		}
		install.Env = env

		return install.Run()
	},
}

// --- version ---

const githubRepo = "sahil-shubham/bhatti"

// checkLatestRelease queries GitHub for the latest release tag.
// Returns empty string on any failure (timeout, network, parse error).
// Caches the result to ~/.bhatti/.latest-version for 1 hour.
func checkLatestRelease() string {
	cacheDir := pkg.DefaultDataDir()
	cachePath := filepath.Join(cacheDir, ".latest-version")

	// Check cache (format: "v1.6.5\n1713980400" = version + unix timestamp)
	if data, err := os.ReadFile(cachePath); err == nil {
		parts := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
		if len(parts) == 2 {
			var ts int64
			fmt.Sscanf(parts[1], "%d", &ts)
			if time.Now().Unix()-ts < 3600 { // 1 hour TTL
				return parts[0]
			}
		}
	}

	// Fetch from GitHub (2s timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("https://api.github.com/repos/" + githubRepo + "/releases/latest")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil || release.TagName == "" {
		return ""
	}

	// Write cache
	os.MkdirAll(cacheDir, 0700)
	content := fmt.Sprintf("%s\n%d", release.TagName, time.Now().Unix())
	os.WriteFile(cachePath, []byte(content), 0600)

	return release.TagName
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version and check for updates",
	Example: `  bhatti version
  bhatti version --json`,
	Run: func(cmd *cobra.Command, args []string) {
		serverVer := ""
		// Quick probe to get server version from header
		if resp, err := apiRequest("GET", "/sandboxes", nil); err == nil {
			resp.Body.Close()
			serverVer = resp.Header.Get("X-Bhatti-Version")
		}

		// Check latest release (cached, 2s timeout)
		latestVer := ""
		if version != "dev" {
			latestVer = checkLatestRelease()
		}

		if isJSON(cmd) {
			out := map[string]string{
				"version": version,
				"api":     apiURL,
			}
			if serverVer != "" {
				out["server_version"] = serverVer
			}
			if latestVer != "" {
				out["latest_version"] = latestVer
			}
			outputJSON(out)
		} else {
			fmt.Printf("bhatti %s\n", version)
			fmt.Printf("api: %s\n", apiURL)
			if serverVer != "" && serverVer != "dev" {
				fmt.Printf("server: %s\n", serverVer)
			}

			// Show update notices
			if version != "dev" && latestVer != "" {
				normVersion := "v" + strings.TrimPrefix(version, "v")
				normLatest := "v" + strings.TrimPrefix(latestVer, "v")
				normServer := ""
				if serverVer != "" && serverVer != "dev" {
					normServer = "v" + strings.TrimPrefix(serverVer, "v")
				}

				if compareVersions(normVersion, normLatest) < 0 {
					fmt.Printf("\nUpdate available: %s \u2192 %s (bhatti update)\n", normVersion, normLatest)
				} else if normServer != "" && compareVersions(normServer, normLatest) < 0 {
					fmt.Printf("\nUpdate available for server: %s \u2192 %s (sudo bhatti update)\n", normServer, normLatest)
				}
			} else if version != "dev" && serverVer != "" && serverVer != "dev" {
				// Fallback: no GitHub info, compare CLI vs server (existing behavior)
				if compareVersions(version, serverVer) < 0 {
					fmt.Printf("\nUpdate available: %s \u2192 %s (bhatti update)\n", version, serverVer)
				}
			}
		}
	},
}

// --- publish / unpublish ---

var publishCmd = &cobra.Command{
	Use:               "publish <sandbox> -p <port> [-a <alias>]",
	Short:             "Publish a sandbox port with a public URL",
	Example: `  bhatti publish dev -p 3000
  bhatti publish dev -p 3000 -a my-app`,
	Args:              exactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	Run: func(cmd *cobra.Command, args []string) {
		port, _ := cmd.Flags().GetInt("port")
		alias, _ := cmd.Flags().GetString("alias")

		body := map[string]interface{}{"port": port}
		if alias != "" {
			body["alias"] = alias
		}
		id, err := resolveID(args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		var result map[string]interface{}
		if err := apiJSON("POST", fmt.Sprintf("/sandboxes/%s/publish", id), body, &result); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		shellFlag, _ := cmd.Flags().GetBool("shell")
		if shellFlag {
			var shellResult map[string]interface{}
			if err := apiJSON("POST", fmt.Sprintf("/sandboxes/%s/shell-token", id), nil, &shellResult); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to generate shell token: %v\n", err)
			} else {
				result["shell_url"] = shellResult["url"]
				result["shell_token"] = shellResult["token"]
			}
		}
		if isJSON(cmd) {
			outputJSON(result)
		} else {
			fmt.Printf("Published: %v\n", result["url"])
			if shellURL, ok := result["shell_url"]; ok {
				fmt.Printf("Shell:     %v\n", shellURL)
			}
		}
	},
}

var unpublishCmd = &cobra.Command{
	Use:               "unpublish <sandbox> -p <port>",
	Short:             "Unpublish a sandbox port",
	Example: `  bhatti unpublish dev -p 3000`,
	Args:              exactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	Run: func(cmd *cobra.Command, args []string) {
		port, _ := cmd.Flags().GetInt("port")
		id, err := resolveID(args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		resp, err := apiRequest("DELETE", fmt.Sprintf("/sandboxes/%s/publish/%d", id, port), nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Status)
			os.Exit(1)
		}
		if !isJSON(cmd) {
			fmt.Printf("Unpublished port %d\n", port)
		}
	},
}

// --- completion ---

var completionCmd = &cobra.Command{
	Use:       "completion <bash|zsh|fish>",
	Short:     "Generate shell completion script",
	Args:      exactArgs(1),
	ValidArgs: []string{"bash", "zsh", "fish"},
	RunE: func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return rootCmd.GenBashCompletion(os.Stdout)
		case "zsh":
			return rootCmd.GenZshCompletion(os.Stdout)
		case "fish":
			return rootCmd.GenFishCompletion(os.Stdout, true)
		default:
			return cmd.Help()
		}
	},
}
