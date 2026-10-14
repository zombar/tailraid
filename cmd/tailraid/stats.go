package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/rpc"
)

func statsCmd() *cobra.Command {
	c := &cobra.Command{Use: "stats", Short: "The Stats page's history kept in the volume (docs/17)"}
	seal := &cobra.Command{
		Use:   "seal",
		Short: "Write this node's partial stats hour into the volume now and roll up any day still missing its rollup",
		Long: `seal asks the node to write the rows it has sampled since the last hour boundary as a
raw segment under /.tailraid/stats/, and to produce the 1-minute and 10-minute rollups of
any day whose raw segments exist without them; with --rollup, today's rollups so far as
well. On the leader it also prunes segments past their retention. It is what a clean
stop does; the hour boundary does it on its own (docs/17 §7).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, addr, _, err := localClient(cmd)
			if err != nil {
				return err
			}
			rollup, _ := cmd.Flags().GetBool("rollup")
			raw, err := c.Admin(ctx, addr, rpc.AdminOp{Op: "stats_seal", Rollup: rollup})
			if err != nil {
				return err
			}
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return printRaw(cmd.OutOrStdout(), raw)
			}
			var res struct {
				Rows int `json:"sealed_rows"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("stats seal reply: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "sealed %d rows\n", res.Rows)
			return nil
		},
	}
	seal.Flags().Bool("rollup", false, "also write today's 1-minute and 10-minute rollups so far (the day's end rewrites them whole)")
	addNodeFlags(seal)
	c.AddCommand(seal)
	return c
}
