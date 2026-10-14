package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/audit"
	"github.com/zombar/tailraid/internal/rpc"
)

// auditCmd reads the audit log (docs/21 §8, docs/12 §12): a user their own records, an
// admin everyone's.
func auditCmd() *cobra.Command {
	c := &cobra.Command{Use: "audit", Short: "The audit log: who did what, and what was refused (docs/21 §8)"}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List audit records, newest first (your own; every user's as an admin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			since, _ := cmd.Flags().GetString("since")
			d, err := parseSince(since)
			if err != nil {
				return err
			}
			user, _ := cmd.Flags().GetString("user")
			kind, _ := cmd.Flags().GetString("kind")
			limit, _ := cmd.Flags().GetInt("limit")
			op := rpc.AdminOp{Op: "audit_ls", User: user, Kind: kind, Limit: limit}
			if d > 0 {
				op.Since = time.Now().Add(-d).UTC().Format(time.RFC3339)
			}
			raw, err := localCall(cmd, op)
			if err != nil {
				return err
			}
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return printRaw(cmd.OutOrStdout(), raw)
			}
			var recs []audit.Record
			if err := json.Unmarshal(raw, &recs); err != nil {
				return fmt.Errorf("audit ls reply: %w", err)
			}
			if len(recs) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no records")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "AT\tNODE\tACTOR\tKIND\tTARGET\tRESULT\tDETAIL")
			for i := range recs {
				r := &recs[i]
				result := r.Result
				if r.Count > 1 {
					result = fmt.Sprintf("%s ×%d", r.Result, r.Count)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", time.Unix(0, r.At).Local().Format("2006-01-02 15:04:05"), r.Node, r.Actor, r.Kind, r.Target, result, r.Detail)
			}
			return tw.Flush()
		},
	}
	ls.Flags().String("since", "7d", "how far back: a duration, with d for days (0: everything kept)")
	ls.Flags().String("user", "", "records of one user (admins)")
	ls.Flags().String("kind", "", "records of one kind, or a prefix ending in a dot (secret.)")
	ls.Flags().Int("limit", 0, "at most this many, newest first (0: all)")
	addNodeFlags(ls)
	c.AddCommand(ls)
	return c
}

// parseSince reads a duration with d for days: 7d, 36h, 0.
func parseSince(s string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("--since %q: want a duration such as 7d or 36h", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("--since %q: want a duration such as 7d or 36h", s)
	}
	return d, nil
}
