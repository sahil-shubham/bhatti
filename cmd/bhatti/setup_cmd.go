package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sahil-shubham/bhatti/pkg"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// --- setup ---

var (
	setupURL   string
	setupToken string
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Configure CLI endpoint and API key",
	Long: `Configure the CLI's API endpoint and key. Writes the result to
~/.bhatti/config.yaml and tests the connection by listing sandboxes.

With no flags, runs interactively (prompts for endpoint and key).
With --url and --token, runs non-interactively — useful for agents,
CI scripts, and provisioning tools that can't answer prompts.`,
	Example: `  # Interactive
  bhatti setup

  # Non-interactive (agents, CI)
  bhatti setup --url https://api.bhatti.sh --token bht_abc123`,
	RunE: func(cmd *cobra.Command, args []string) error {
		var endpoint, key string

		// Non-interactive path: both flags must be set together. If only
		// one is set, we still drop into prompts to fill in the other —
		// keeps the flag affordance lenient without surprising scripts.
		nonInteractive := setupURL != "" && setupToken != ""

		if nonInteractive {
			endpoint = setupURL
			key = strings.TrimSpace(setupToken)
		} else {
			if setupURL != "" {
				endpoint = setupURL
			} else {
				fmt.Printf("API endpoint [%s]: ", apiURL)
				var in string
				fmt.Scanln(&in)
				if in == "" {
					endpoint = apiURL
				} else {
					endpoint = in
				}
			}

			// Tolerate trailing slashes — we concatenate paths like
			// "/sandboxes" later, and "http://x//sandboxes" works on most
			// servers but breaks strict ones (and looks wrong in logs).
			endpoint = strings.TrimRight(endpoint, "/")

			if setupToken != "" {
				key = strings.TrimSpace(setupToken)
			} else {
				fmt.Print("API key: ")
				keyBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
				fmt.Println()
				if err != nil {
					return fmt.Errorf("read key: %w", err)
				}
				key = strings.TrimSpace(string(keyBytes))
			}
		}

		if key == "" {
			return fmt.Errorf("API key is required (pass --token or enter at the prompt)")
		}

		// Write config to the *invoking* user's home, even if we're running
		// under sudo. pkg.DefaultDataDir() honors SUDO_USER, so this resolves
		// to the same path that a later non-sudo `bhatti list` will read.
		cfgDir := pkg.DefaultDataDir()
		if err := os.MkdirAll(cfgDir, 0700); err != nil {
			return fmt.Errorf("create config dir %s: %w", cfgDir, err)
		}
		cfgPath := filepath.Join(cfgDir, "config.yaml")

		var cfgContent string
		if strings.HasPrefix(endpoint, "https://") || strings.HasPrefix(endpoint, "http://") {
			// Remote endpoint — save URL and token
			cfgContent = fmt.Sprintf("api_url: %s\nauth_token: %s\n", endpoint, key)
		} else {
			// Local — save listen address and token
			cfgContent = fmt.Sprintf("listen: %s\nauth_token: %s\n", endpoint, key)
		}

		if err := os.WriteFile(cfgPath, []byte(cfgContent), 0600); err != nil {
			return fmt.Errorf("write config: %w", err)
		}

		// If we're root via sudo, hand the dir+file back to the real user.
		// Otherwise the next non-sudo `bhatti` invocation will hit EACCES
		// on a root-owned ~/.bhatti and the user will (incorrectly) reach
		// for sudo again — the loop we're trying to break.
		pkg.EnsureUserOwnedPath(cfgDir, cfgPath)
		if os.Getenv("SUDO_USER") != "" && os.Getenv("SUDO_USER") != "root" {
			fmt.Fprintf(os.Stderr, "note: you don't need sudo for `bhatti setup` —\n"+
				"      saved config for user %q so it works without sudo next time.\n",
				os.Getenv("SUDO_USER"))
		}
		fmt.Printf("Saved to %s\n", cfgPath)

		// Test connection using an authenticated endpoint.
		// /health is unauthenticated — it only proves the server is
		// reachable, not that the API key is valid. Use /sandboxes
		// (GET) which requires auth, so we catch bad keys immediately.
		fmt.Print("Testing connection... ")
		apiURL = endpoint
		apiToken = key
		var sandboxes []any
		if err := apiJSON("GET", "/sandboxes", nil, &sandboxes); err != nil {
			fmt.Printf("✗ %v\n", err)
			if nonInteractive {
				return fmt.Errorf("authentication failed: %w", err)
			}
			return nil
		}
		fmt.Printf("✓ authenticated (%d sandboxes)\n", len(sandboxes))

		// Skip the completions hint when run non-interactively — it's
		// noise to a script that just wanted to write the config.
		if nonInteractive {
			return nil
		}
		shell := os.Getenv("SHELL")
		switch {
		case strings.HasSuffix(shell, "/zsh"):
			fmt.Println("\nEnable completions:")
			fmt.Println("  echo 'source <(bhatti completion zsh)' >> ~/.zshrc")
		case strings.HasSuffix(shell, "/bash"):
			fmt.Println("\nEnable completions:")
			fmt.Println("  echo 'source <(bhatti completion bash)' >> ~/.bashrc")
		case strings.HasSuffix(shell, "/fish"):
			fmt.Println("\nEnable completions:")
			fmt.Println("  bhatti completion fish > ~/.config/fish/completions/bhatti.fish")
		case os.Getenv("PSModulePath") != "":
			// PowerShell exports PSModulePath into every child process on
			// Windows, macOS, and Linux. SHELL is unset on Windows and on
			// *nix points at the user's login shell (usually zsh/bash), so
			// this branch only fires when the previous cases didn't match
			// — i.e. native Windows or someone running `pwsh` directly.
			fmt.Println("\nEnable completions:")
			fmt.Println("  bhatti completion powershell >> $PROFILE")
		}
		return nil
	},
}

func init() {
	setupCmd.Flags().StringVar(&setupURL, "url", "", "API endpoint URL (skips the prompt when set with --token)")
	setupCmd.Flags().StringVar(&setupToken, "token", "", "API key (skips the prompt when set with --url)")
}
