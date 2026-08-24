package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// --- secret ---

var secretCmd = &cobra.Command{
	Use:   "secret <set|list|delete>",
	Short: "Manage encrypted secrets",
	Long: `Secrets are encrypted at rest (age) and scoped to your API key.
They can be referenced in templates and injected into sandboxes at boot.`,
	Example: `  bhatti secret set API_KEY sk-abc123
  bhatti secret list
  bhatti secret delete API_KEY`,
}

var secretSetCmd = &cobra.Command{
	Use:   "set <name> <value>",
	Short: "Create or update a secret",
	Args:  exactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		if err := apiJSON("POST", "/secrets", map[string]any{
			"name": args[0], "value": args[1],
		}, nil); err != nil {
			return err
		}
		fmt.Println("ok")
		return nil
	},
}

var secretListCmd = &cobra.Command{
	Use:   "list",
	Short: "List secrets",
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		var secrets []struct {
			Name string `json:"name"`
		}
		if err := apiJSON("GET", "/secrets", nil, &secrets); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(secrets)
		} else {
			for _, s := range secrets {
				fmt.Println(s.Name)
			}
		}
		return nil
	},
}

var secretDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a secret",
	Args:  exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		if !confirmAction(cmd, fmt.Sprintf("Delete secret %q?", args[0])) {
			return errAborted
		}

		if err := apiJSON("DELETE", "/secrets/"+args[0], nil, nil); err != nil {
			return err
		}
		fmt.Println("deleted")
		return nil
	},
}

func init() {
	secretCmd.AddCommand(secretSetCmd)
	secretCmd.AddCommand(secretListCmd)
	secretDeleteCmd.Flags().BoolP("yes", "y", false, "Skip confirmation")
	secretCmd.AddCommand(secretDeleteCmd)
}
