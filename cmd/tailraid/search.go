package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/search"
)

func searchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "search <words>...",
		Short: "Find files by name, audio tags and text contents (docs/18)",
		Long: `search asks this node's index: names from the live tree, tags and text contents
from the index the leader keeps under /.tailraid/index. The words are ANDed and the last
matches as a prefix. "tailraid search index" runs the leader's pass now; to search for
the word index, quote it with another word or use --under.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, addr, _, err := localClient(ctx, cmd)
			if err != nil {
				return err
			}
			under, _ := cmd.Flags().GetString("under")
			limit, _ := cmd.Flags().GetInt("limit")
			if limit < 1 || limit > search.MaxLimit {
				return fmt.Errorf("--limit %d: want 1..%d", limit, search.MaxLimit)
			}
			raw, err := c.Admin(ctx, addr, rpc.AdminOp{Op: "search", Query: strings.Join(args, " "), Path: under, Limit: limit})
			if err != nil {
				return err
			}
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return printRaw(cmd.OutOrStdout(), raw)
			}
			var res search.Result
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("search reply: %w", err)
			}
			w := cmd.OutOrStdout()
			if res.Index != "" {
				fmt.Fprintf(w, "(index %s: names only)\n", res.Index)
			}
			if len(res.Hits) == 0 {
				fmt.Fprintf(w, "no matches for %q\n", res.Query)
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			for i := range res.Hits {
				h := &res.Hits[i]
				what := h.Match
				if h.Title != "" {
					what += ": " + strings.TrimSpace(h.Title+" · "+h.Artist)
				}
				fmt.Fprintf(tw, "%s\t%s\n", h.Path, what)
			}
			return tw.Flush()
		},
	}
	c.Flags().String("under", "", "only hits inside this folder")
	c.Flags().Int("limit", search.DefaultLimit, fmt.Sprintf("the most hits (1..%d)", search.MaxLimit))
	addNodeFlags(c)
	index := &cobra.Command{
		Use:   "index",
		Short: "Run the leader's index pass now and print what it did",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := adminCall(cmd, rpc.AdminOp{Op: "index_now"})
			if err != nil {
				return err
			}
			return printRaw(cmd.OutOrStdout(), raw)
		},
	}
	addNodeFlags(index)
	c.AddCommand(index)
	return c
}
