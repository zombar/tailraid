package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/rpc"
)

func chunkCmd() *cobra.Command {
	c := &cobra.Command{Use: "chunk", Short: "Inspect chunks"}
	describe := &cobra.Command{
		Use:   "describe <hash|/path>",
		Short: "State, placement, referencing files and (with dev.enabled) the history of a chunk, or of every chunk of a file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// answered by the node asked (its own event ring), not forwarded to the leader
			cl, addr, _, err := localClient(cmd.Context(), cmd)
			if err != nil {
				return err
			}
			raw, err := cl.Admin(cmd.Context(), addr, rpc.AdminOp{Op: "chunk", Chunk: args[0]})
			if err != nil {
				return err
			}
			if j, _ := cmd.Flags().GetBool("json"); j {
				return printRaw(cmd.OutOrStdout(), raw)
			}
			var reps []admin.ChunkReport
			if err := json.Unmarshal(raw, &reps); err != nil {
				return err
			}
			for i := range reps {
				if i > 0 {
					fmt.Fprintln(cmd.OutOrStdout())
				}
				printChunk(cmd.OutOrStdout(), i, reps[i])
			}
			return nil
		},
	}
	addNodeFlags(describe)
	c.AddCommand(describe)
	return c
}

func printChunk(w io.Writer, idx int, r admin.ChunkReport) {
	fmt.Fprintf(w, "chunk %d  %s\n", idx, r.Hash)
	if !r.Found {
		fmt.Fprintf(w, "  state: %s\n", r.State)
		return
	}
	fmt.Fprintf(w, "  state: %s   size: %d   policy: %d+%d%s   refs: %d   live slots: %d/%d\n", r.State, r.Size, r.K, r.M,
		map[bool]string{true: " (stale)", false: ""}[r.PolicyStale], r.Refs, r.LiveSlots, len(r.Slots))
	if r.TombstonedAt != "" {
		fmt.Fprintf(w, "  tombstoned: %s\n", r.TombstonedAt)
	}
	tw := tabwriter.NewWriter(w, 2, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  SLOT\tNODE\tLIVE\tSHARD")
	for _, s := range r.Slots {
		node, live, shard := "-", "-", "-"
		if s.Node != 0 {
			node = fmt.Sprintf("%d %s", s.Node, s.Name)
			live = map[bool]string{true: "yes", false: "no"}[s.Live]
			if s.Present != nil {
				shard = map[bool]string{true: "present", false: "MISSING"}[*s.Present]
			}
		}
		fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\n", s.Idx, node, live, shard)
	}
	_ = tw.Flush()
	if len(r.Files) > 0 {
		fmt.Fprintf(w, "  files: %s\n", strings.Join(r.Files, ", "))
	}
	switch {
	case !r.EventRing:
		fmt.Fprintln(w, "  history: (event ring disabled)")
	case len(r.Events) == 0:
		fmt.Fprintln(w, "  history: (no events on this node)")
	default:
		fmt.Fprintln(w, "  history:")
		for _, e := range r.Events {
			fmt.Fprintf(w, "    %s  n%d  %-14s %s\n", e.At.UTC().Format("15:04:05.000"), e.Node, e.Kind, e.Detail)
		}
	}
}
