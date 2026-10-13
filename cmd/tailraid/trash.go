package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/rpc"
)

// trashCmd is the terminal's view of the trash (docs/12 §10): what a delete left
// behind, a restore, and the purge that reclaims space before the retention would.
func trashCmd() *cobra.Command {
	c := &cobra.Command{Use: "trash", Short: "List, restore or purge deleted entries"}
	c.AddCommand(trashLsCmd(), trashRestoreCmd(), trashPurgeCmd())
	return c
}

// trashPageBytes is the JSON a `trash ls` page may carry: well within the 4 MiB the RPC
// client reads of an admin reply.
const trashPageBytes = 1 << 20

func trashLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls [path]",
		Short: "List the deleted entries, every one or those under a folder",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			op := rpc.AdminOp{Op: "trash_ls", PageBytes: trashPageBytes}
			if len(args) == 1 {
				op.Path = args[0]
			}
			// a page at a time: one reply of a large trash outgrew what a client reads
			// (I-234); a node that does not page answers everything with no "next"
			var all []admin.TrashEntry
			for {
				raw, err := adminCall(cmd, op)
				if err != nil {
					return err
				}
				var rep struct {
					Entries []admin.TrashEntry `json:"entries"`
					Next    string             `json:"next"`
				}
				if err := json.Unmarshal(raw, &rep); err != nil {
					return err
				}
				all = append(all, rep.Entries...)
				if rep.Next == "" || rep.Next == op.Cursor {
					break
				}
				op.Cursor = rep.Next
			}
			return printTrash(cmd.OutOrStdout(), all)
		},
	}
	addNodeFlags(c)
	return c
}

func printTrash(w io.Writer, entries []admin.TrashEntry) error {
	if len(entries) == 0 {
		_, err := fmt.Fprintln(w, "the trash is empty")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "INO\tDELETED\tAGE\tSIZE\tTYPE\tPATH")
	for _, e := range entries {
		age := "?"
		if at, err := time.Parse(time.RFC3339, e.DeletedAt); err == nil {
			age = time.Since(at).Truncate(time.Minute).String()
		}
		p := e.Path
		if !e.Restorable {
			p += " (its folder is deleted too)"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", e.Ino, e.DeletedAt, age, iec(e.Size), e.Type, p)
	}
	return tw.Flush()
}

// iec prints bytes in binary units with one decimal, as the browser UI does.
func iec(n uint64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatUint(n, 10) + " B"
	}
	v, suffix := float64(n), "KMGTPE"
	i := 0
	for v >= unit && i < len(suffix) {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %ciB", v, suffix[i-1])
}

func trashRestoreCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "restore <ino>",
		Short: "Put a deleted entry back, in its place or where --to and --as say",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ino, err := strconv.ParseUint(args[0], 10, 64)
			if err != nil || ino == 0 {
				return fmt.Errorf("trash restore: %q is not an inode number (see `tailraid trash ls`)", args[0])
			}
			to, _ := cmd.Flags().GetString("to")
			as, _ := cmd.Flags().GetString("as")
			raw, err := adminCall(cmd, rpc.AdminOp{Op: "trash_restore", Ino: ino, To: to, As: as})
			if err != nil {
				return err
			}
			return printRaw(cmd.OutOrStdout(), raw)
		},
	}
	c.Flags().String("to", "", "restore into this folder instead of the original one")
	c.Flags().String("as", "", "restore under this name instead of the original one")
	addNodeFlags(c)
	return c
}

func trashPurgeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "purge",
		Short: "Delete entries from the trash for good: one by inode, those older than a duration, or all",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ino, _ := cmd.Flags().GetUint64("ino")
			older, _ := cmd.Flags().GetString("older-than")
			all, _ := cmd.Flags().GetBool("all")
			if ino == 0 && older == "" && !all {
				return errors.New("trash purge: one of --ino, --older-than or --all is required")
			}
			raw, err := adminCall(cmd, rpc.AdminOp{Op: "trash_purge", Ino: ino, OlderThan: older, All: all})
			if err != nil {
				return err
			}
			return printRaw(cmd.OutOrStdout(), raw)
		},
	}
	c.Flags().Uint64("ino", 0, "the one entry to purge")
	c.Flags().String("older-than", "", "purge every entry deleted longer than this ago (0 for all)")
	c.Flags().Bool("all", false, "purge every entry")
	addNodeFlags(c)
	return c
}
