package main

import (
	"fmt"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// --- secret ---

var secretCmd = &cobra.Command{
	Use:   "secret <set|list|delete|grant|grants|revoke>",
	Short: "Manage encrypted secrets and sandbox grants",
	Long: `Secrets are encrypted at rest (age) and scoped to your API key.
A grant gives a sandbox a placeholder instead of the real secret. netd swaps
it for the real value only in HTTPS request headers to the granted hosts.`,
	Example: `  bhatti secret set GH_TOKEN ghp_example
  bhatti secret grant GH_TOKEN --sandbox dev --host api.github.com
  bhatti secret grants
  bhatti secret revoke <grant-id>
  bhatti secret list`,
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

type secretGrant struct {
	ID          string   `json:"id"`
	Secret      string   `json:"secret"`
	SandboxID   string   `json:"sandbox_id"`
	SandboxName string   `json:"sandbox_name"`
	Hosts       []string `json:"hosts"`
	Placeholder string   `json:"placeholder"`
	CreatedAt   string   `json:"created_at"`
	ExpiresAt   string   `json:"expires_at,omitempty"`
	RevokedAt   string   `json:"revoked_at,omitempty"`
	Status      string   `json:"status"`
}

func (g secretGrant) sandboxLabel() string {
	if g.SandboxName != "" {
		return g.SandboxName
	}
	return g.SandboxID
}

func (g secretGrant) expiryLabel() string {
	if g.ExpiresAt == "" {
		return "never"
	}
	if expires, err := time.Parse(time.RFC3339Nano, g.ExpiresAt); err == nil {
		return expires.Local().Format(time.RFC3339)
	}
	return g.ExpiresAt
}

var secretGrantCmd = &cobra.Command{
	Use:     "grant <name>",
	Short:   "Allow a sandbox to use a secret at specific HTTPS hosts",
	Example: "  bhatti secret grant GH_TOKEN --sandbox dev --host api.github.com --host uploads.github.com --ttl 7d",
	Args:    exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		sandbox, _ := cmd.Flags().GetString("sandbox")
		hosts, _ := cmd.Flags().GetStringArray("host")
		if sandbox == "" {
			return fmt.Errorf("--sandbox is required")
		}
		if len(hosts) == 0 {
			return fmt.Errorf("at least one --host is required")
		}
		for _, host := range hosts {
			if host == "" {
				return fmt.Errorf("--host cannot be empty")
			}
		}
		ttl, _ := cmd.Flags().GetString("ttl")
		body := map[string]any{"sandbox": sandbox, "hosts": hosts}
		if ttl != "" {
			body["ttl"] = ttl
		}
		var grant secretGrant
		if err := apiJSON("POST", "/secrets/"+url.PathEscape(args[0])+"/grants", body, &grant); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(grant)
		} else {
			fmt.Printf("grant/%s %s → %s for %s (expires %s)\n",
				grant.ID, args[0], grant.sandboxLabel(), strings.Join(grant.Hosts, ", "), grant.expiryLabel())
			fmt.Printf("  Placeholder: %s\n", grant.Placeholder)
			fmt.Printf("  Use the placeholder wherever the sandbox sends %s (e.g. export %s=%s).\n",
				args[0], args[0], grant.Placeholder)
		}
		return nil
	},
}

var secretGrantsCmd = &cobra.Command{
	Use:   "grants",
	Short: "List all secret grants (including expired and revoked)",
	Args:  exactArgs(0),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		var grants []secretGrant
		if err := apiJSON("GET", "/secrets/grants", nil, &grants); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(grants)
		} else {
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tSECRET\tSANDBOX\tHOSTS\tSTATUS\tEXPIRES")
			for _, grant := range grants {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", grant.ID, grant.Secret,
					grant.sandboxLabel(), strings.Join(grant.Hosts, ", "), grant.Status, grant.expiryLabel())
			}
			return w.Flush()
		}
		return nil
	},
}

var secretRevokeCmd = &cobra.Command{
	Use:   "revoke <id>",
	Short: "Revoke a secret grant",
	Long: `Revoke a secret grant. Revocation stops new connections immediately;
connections already open keep the value until they close.`,
	Args: exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		if !confirmAction(cmd, fmt.Sprintf("Revoke secret grant %q?", args[0])) {
			return errAborted
		}
		if err := apiJSON("DELETE", "/secrets/grants/"+url.PathEscape(args[0]), nil, nil); err != nil {
			return err
		}
		fmt.Println("revoked")
		return nil
	},
}

func init() {
	secretCmd.AddCommand(secretSetCmd)
	secretCmd.AddCommand(secretListCmd)
	secretDeleteCmd.Flags().BoolP("yes", "y", false, "Skip confirmation")
	secretCmd.AddCommand(secretDeleteCmd)
	secretGrantCmd.Flags().String("sandbox", "", "Sandbox name or ID (required)")
	secretGrantCmd.Flags().StringArray("host", nil, "Allowed HTTPS host, exact or *.wildcard (repeatable; required)")
	secretGrantCmd.Flags().String("ttl", "", "Grant lifetime, e.g. 90m, 24h, 7d (default: until revoked or sandbox destroyed)")
	secretGrantCmd.RegisterFlagCompletionFunc("sandbox", func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// The grant name is positional; it must not suppress sandbox flag completion.
		return completeSandboxNames(cmd, nil, toComplete)
	})
	secretCmd.AddCommand(secretGrantCmd)
	secretCmd.AddCommand(secretGrantsCmd)
	secretRevokeCmd.Flags().BoolP("yes", "y", false, "Skip confirmation")
	secretCmd.AddCommand(secretRevokeCmd)
}
