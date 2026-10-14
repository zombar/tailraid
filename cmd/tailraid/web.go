package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/rpc"
)

// webCmd signs a browser in to the volume (docs/21 §6, docs/12 §12): `login` prints a
// one-time link, `logout` ends sessions.
func webCmd() *cobra.Command {
	c := &cobra.Command{Use: "web", Short: "Sign a browser in to the volume"}
	login := &cobra.Command{
		Use:   "login",
		Short: "Print a one-time link that signs a browser in to the volume, on every node's page, for users.session",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := localCall(cmd, rpc.AdminOp{Op: "web_login"})
			if err != nil {
				return err
			}
			var rep struct {
				URL       string `json:"url"`
				ExpiresAt string `json:"expires_at"`
			}
			if err := json.Unmarshal(raw, &rep); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Open this link within five minutes; it works once:\n\n  %s\n\nThe session lasts until %s on every node's page; `tailraid web logout` ends it.\n", rep.URL, rep.ExpiresAt)
			return nil
		},
	}
	login.Flags().String("node", "", "RPC address of the node whose page the link opens (default: this device)")
	logout := &cobra.Command{
		Use:   "logout",
		Short: "End the browser sessions this device's key minted; --all ends every session of yours",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			all, _ := cmd.Flags().GetBool("all")
			raw, err := localCall(cmd, rpc.AdminOp{Op: "web_logout", All: all})
			if err != nil {
				return err
			}
			var rep struct {
				Ended int `json:"ended"`
			}
			if err := json.Unmarshal(raw, &rep); err != nil {
				return err
			}
			if all {
				fmt.Fprintln(cmd.OutOrStdout(), "every browser session of yours has ended")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d browser session(s) from this device ended\n", rep.Ended)
			return nil
		},
	}
	logout.Flags().Bool("all", false, "end every session of yours, from every device")
	c.AddCommand(login, logout)
	return c
}
