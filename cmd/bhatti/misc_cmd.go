package main

import (
	"fmt"
	"os"
	"os/exec"

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

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version and API endpoint",
	Example: `  bhatti version
  bhatti version --json`,
	Run: func(cmd *cobra.Command, args []string) {
		serverVer := ""
		// Quick probe to get server version from header
		if resp, err := apiRequest("GET", "/sandboxes", nil); err == nil {
			resp.Body.Close()
			serverVer = resp.Header.Get("X-Bhatti-Version")
		}

		if isJSON(cmd) {
			out := map[string]string{
				"version": version,
				"api":     apiURL,
			}
			if serverVer != "" {
				out["server_version"] = serverVer
			}
			outputJSON(out)
		} else {
			fmt.Printf("bhatti %s\n", version)
			fmt.Printf("api: %s\n", apiURL)
			if serverVer != "" && serverVer != "dev" {
				fmt.Printf("server: %s\n", serverVer)
				if version != "dev" && compareVersions(version, serverVer) < 0 {
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
