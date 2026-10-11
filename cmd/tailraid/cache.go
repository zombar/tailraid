package main

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/pins"
	"github.com/zombar/tailraid/internal/rpc"
)

// cacheCmd manages the folders this device keeps in its chunk cache (docs/12 §10,
// I-162). Pins are the node's own state, so every call goes to this node (or --node),
// never to the leader.
func cacheCmd() *cobra.Command {
	c := &cobra.Command{Use: "cache", Short: "Keep folders on this device: pin, unpin, list"}
	pin := &cobra.Command{
		Use:   "pin <path>",
		Short: "Fetch every chunk under the folder into this device's cache and keep it there",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := localCall(cmd, rpc.AdminOp{Op: "cache_pin", Path: args[0]})
			if err != nil {
				return err
			}
			return printRaw(cmd.OutOrStdout(), raw)
		},
	}
	unpin := &cobra.Command{
		Use:   "unpin <path>",
		Short: "Release the folder: its chunks return to the cache's ordinary eviction",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := localCall(cmd, rpc.AdminOp{Op: "cache_unpin", Path: args[0]})
			if err != nil {
				return err
			}
			return printRaw(cmd.OutOrStdout(), raw)
		},
	}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "The cache's size and use, and each pinned folder with how much of it has arrived",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := localCall(cmd, rpc.AdminOp{Op: "cache_ls"})
			if err != nil {
				return err
			}
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return printRaw(cmd.OutOrStdout(), raw)
			}
			var rep pins.Report
			if err := json.Unmarshal(raw, &rep); err != nil {
				return err
			}
			return printPins(cmd.OutOrStdout(), rep)
		},
	}
	for _, sub := range []*cobra.Command{pin, unpin, ls} {
		addNodeFlags(sub)
	}
	c.AddCommand(pin, unpin, ls)
	return c
}

func printPins(w io.Writer, rep pins.Report) error {
	fmt.Fprintf(w, "cache %s used of %s, %s pinned\n", iec(uint64(rep.Used)), iec(uint64(rep.Size)), iec(uint64(rep.PinnedBytes)))
	if len(rep.Pins) == 0 {
		_, err := fmt.Fprintln(w, "no pinned folders")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PATH\tBYTES\tCACHED")
	for _, p := range rep.Pins {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Path, iec(p.Bytes), iec(p.CachedBytes))
	}
	return tw.Flush()
}

// localCall sends an admin op to this device's node (or --node), not the leader.
func localCall(cmd *cobra.Command, op rpc.AdminOp) (json.RawMessage, error) {
	c, addr, _, err := localClient(cmd.Context(), cmd)
	if err != nil {
		return nil, err
	}
	return c.Admin(cmd.Context(), addr, op)
}
