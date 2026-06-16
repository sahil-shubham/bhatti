package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"
)

// forwardCmd forwards a host port to a port inside the sandbox (kubectl
// port-forward style). The daemon binds 127.0.0.1:<host-port> and bridges each
// connection to the guest port over the vsock tunnel; the forward is torn down
// when this command exits (Ctrl-C) or the sandbox is destroyed.
var forwardCmd = &cobra.Command{
	Use:   "forward <sandbox> <guest-port> [host-port]",
	Short: "Forward a host port to a port inside the sandbox",
	Example: `  bhatti forward dev 3000          # 127.0.0.1:<random> -> guest:3000
  bhatti forward dev 5432 5432     # 127.0.0.1:5432 -> guest:5432`,
	Args:              cobra.RangeArgs(2, 3),
	ValidArgsFunction: completeSandboxNames,
	Run: func(cmd *cobra.Command, args []string) {
		guestPort, err := strconv.Atoi(args[1])
		if err != nil || guestPort < 1 || guestPort > 65535 {
			fmt.Fprintf(os.Stderr, "Error: invalid guest port %q\n", args[1])
			os.Exit(1)
		}
		hostPort := 0
		if len(args) > 2 {
			if hostPort, err = strconv.Atoi(args[2]); err != nil || hostPort < 0 || hostPort > 65535 {
				fmt.Fprintf(os.Stderr, "Error: invalid host port %q\n", args[2])
				os.Exit(1)
			}
		}
		id, err := resolveID(args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

		var res map[string]interface{}
		body := map[string]interface{}{"guest_port": guestPort, "host_port": hostPort}
		if err := apiJSON("POST", fmt.Sprintf("/sandboxes/%s/forward", id), body, &res); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if isJSON(cmd) {
			outputJSON(res)
			return
		}
		hostAddr, _ := res["host_addr"].(string)
		fmt.Printf("Forwarding %s -> guest:%d  (Ctrl-C to stop)\n", hostAddr, guestPort)

		// Hold the forward open until interrupted, then tear it down.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		fmt.Fprintln(os.Stderr, "\nStopping forward...")
		apiRequest("DELETE", fmt.Sprintf("/sandboxes/%s/forward", id), nil)
	},
}
