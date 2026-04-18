package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sahil-shubham/bhatti/pkg"
	"github.com/sahil-shubham/bhatti/pkg/store"
	"github.com/spf13/cobra"
)

// --- user (local commands — operate on SQLite directly) ---

var userCmd = &cobra.Command{
	Use:   "user <create|list|delete|rotate-key>",
	Short: "Manage users (requires DB access)",
	Long: `User management operates directly on the local SQLite database.
Run on the server, not remotely.`,
	Example: `  sudo bhatti user create --name alice --max-sandboxes 10
  sudo bhatti user list
  sudo bhatti user rotate-key alice
  sudo bhatti user delete alice`,
}

var userCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new user",
	RunE: func(cmd *cobra.Command, args []string) error {
		name, _ := cmd.Flags().GetString("name")
		maxSandboxes, _ := cmd.Flags().GetInt("max-sandboxes")
		maxCPUs, _ := cmd.Flags().GetInt("max-cpus")
		maxMemory, _ := cmd.Flags().GetInt("max-memory")

		if name == "" {
			return fmt.Errorf("--name is required")
		}

		st := openLocalStore()
		defer st.Close()

		apiKey := generateAPIKey()
		keyHash := sha256HexCLI(apiKey)

		subnetIdx, err := st.NextSubnetIndex()
		if err != nil {
			return fmt.Errorf("subnet index: %w", err)
		}

		idBytes := make([]byte, 4)
		rand.Read(idBytes)
		userID := "usr_" + hex.EncodeToString(idBytes)

		u := store.User{
			ID:                    userID,
			Name:                  name,
			APIKeyHash:            keyHash,
			MaxSandboxes:          maxSandboxes,
			MaxCPUsPerSandbox:     maxCPUs,
			MaxMemoryMBPerSandbox: maxMemory,
			SubnetIndex:           subnetIdx,
			CreatedAt:             time.Now(),
		}

		if err := st.CreateUser(u); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return fmt.Errorf("user %q already exists", name)
			}
			return fmt.Errorf("create user: %w", err)
		}
		st.InsertEvents([]store.Event{{
			Type: "user.created", UserID: userID,
			Meta: map[string]any{"name": name, "max_sandboxes": maxSandboxes, "subnet_index": subnetIdx},
		}})

		fmt.Printf("User created:\n")
		fmt.Printf("  ID:       %s\n", u.ID)
		fmt.Printf("  Name:     %s\n", u.Name)
		fmt.Printf("  Subnet:   %d\n", u.SubnetIndex)
		fmt.Printf("  API key:  %s\n", apiKey)
		fmt.Println()
		fmt.Println("This key will not be shown again. Save it now.")
		fmt.Printf("\nQuick start:\n")
		fmt.Printf("  export BHATTI_URL=%s\n", apiURL)
		fmt.Printf("  export BHATTI_TOKEN=%s\n", apiKey)
		fmt.Printf("  bhatti create --name my-sandbox\n")
		return nil
	},
}

var userListCmd = &cobra.Command{
	Use:   "list",
	Short: "List users",
	RunE: func(cmd *cobra.Command, args []string) error {
		st := openLocalStore()
		defer st.Close()

		users, err := st.ListUsers()
		if err != nil {
			return fmt.Errorf("list users: %w", err)
		}
		if isJSON(cmd) {
			outputJSON(users)
		} else {
			fmt.Printf("%-12s %-20s %-8s %-6s %-6s %-8s\n",
				"ID", "NAME", "SANDBOXES", "CPUS", "MEM", "SUBNET")
			for _, u := range users {
				count, _ := st.CountUserSandboxes(u.ID)
				fmt.Printf("%-12s %-20s %d/%-6d %-6d %-6d %-8d\n",
					u.ID, u.Name, count, u.MaxSandboxes,
					u.MaxCPUsPerSandbox, u.MaxMemoryMBPerSandbox, u.SubnetIndex)
			}
		}
		return nil
	},
}

var userDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a user",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !confirmAction(cmd, fmt.Sprintf("Delete user %q?", args[0])) {
			return nil
		}

		st := openLocalStore()
		defer st.Close()

		users, _ := st.ListUsers()
		var userID string
		for _, u := range users {
			if u.Name == args[0] {
				userID = u.ID
				break
			}
		}
		if userID == "" {
			return fmt.Errorf("user %q not found", args[0])
		}
		if err := st.DeleteUser(userID); err != nil {
			return err
		}
		st.InsertEvents([]store.Event{{
			Type: "user.deleted", UserID: userID,
			Meta: map[string]any{"name": args[0]},
		}})
		fmt.Printf("deleted user %q\n", args[0])
		return nil
	},
}

var userRotateKeyCmd = &cobra.Command{
	Use:   "rotate-key <name>",
	Short: "Rotate a user's API key",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		st := openLocalStore()
		defer st.Close()

		users, _ := st.ListUsers()
		var userID string
		for _, u := range users {
			if u.Name == args[0] {
				userID = u.ID
				break
			}
		}
		if userID == "" {
			return fmt.Errorf("user %q not found", args[0])
		}

		newKey := generateAPIKey()
		newHash := sha256HexCLI(newKey)

		if err := st.RotateUserKey(userID, newHash); err != nil {
			return err
		}
		st.InsertEvents([]store.Event{{
			Type: "user.key_rotated", UserID: userID,
			Meta: map[string]any{"name": args[0]},
		}})

		fmt.Printf("API key rotated for %q\n", args[0])
		fmt.Printf("  New key: %s\n", newKey)
		fmt.Println()
		fmt.Println("The old key is immediately invalidated.")
		fmt.Println("This key will not be shown again. Save it now.")
		return nil
	},
}

func init() {
	userCreateCmd.Flags().String("name", "", "User name (required)")
	userCreateCmd.Flags().Int("max-sandboxes", 5, "Max sandboxes for this user")
	userCreateCmd.Flags().Int("max-cpus", 4, "Max CPUs per sandbox")
	userCreateCmd.Flags().Int("max-memory", 4096, "Max memory MB per sandbox")

	userCmd.AddCommand(userCreateCmd)
	userCmd.AddCommand(userListCmd)
	userDeleteCmd.Flags().BoolP("yes", "y", false, "Skip confirmation")
	userCmd.AddCommand(userDeleteCmd)
	userCmd.AddCommand(userRotateKeyCmd)
}

// openLocalStore opens the SQLite store for user admin commands.
//
// Resolution order for data_dir:
//  1. --data-dir flag (explicit)
//  2. config file's data_dir field (from LoadConfig)
//  3. ~/.bhatti (default)
//
// On a server, LoadConfig finds /etc/bhatti/config.yaml which has
// data_dir: /var/lib/bhatti, so the correct state.db is used without
// any hardcoded fallbacks.
func openLocalStore() *store.Store {
	// Check --data-dir flag first
	dataDir, _ := rootCmd.PersistentFlags().GetString("data-dir")

	if dataDir == "" {
		cfg, err := pkg.LoadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "load config: %v\n", err)
			os.Exit(1)
		}
		dataDir = cfg.DataDir
	}

	dbPath := filepath.Join(dataDir, "state.db")
	st, err := store.New(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store %s: %v\n", dbPath, err)
		os.Exit(1)
	}
	return st
}

func generateAPIKey() string {
	b := make([]byte, 32)
	rand.Read(b)
	return "bht_" + hex.EncodeToString(b)
}

func sha256HexCLI(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
