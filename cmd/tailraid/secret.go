package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/secrets"
)

func secretCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "secret",
		Short: "The cluster's secrets store: values sealed in the metadata, the key in each device's keychain (docs/19)",
	}
	set := &cobra.Command{
		Use:   "set <name>",
		Short: "Store a secret; the value is read from stdin (piped, or typed without echo), never from an argument",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			value, err := readSecret(cmd)
			if err != nil {
				return err
			}
			if _, err := localCall(cmd, rpc.AdminOp{Op: "secret_set", Name: args[0], Value: value}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stored %s\n", args[0])
			return nil
		},
	}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List the secrets: names, when and by which node each was set, and whether this node can open it; never a value",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := localCall(cmd, rpc.AdminOp{Op: "secret_ls"})
			if err != nil {
				return err
			}
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return printRaw(cmd.OutOrStdout(), raw)
			}
			var es []secrets.Entry
			if err := json.Unmarshal(raw, &es); err != nil {
				return fmt.Errorf("secret ls reply: %w", err)
			}
			if len(es) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no secrets")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSET\tBY\tTHIS NODE")
			for _, e := range es {
				state := "readable"
				if !e.Readable {
					state = "unreadable"
				}
				fmt.Fprintf(tw, "%s\t%s\tnode %d\t%s\n", e.Name, time.Unix(0, e.At).Local().Format("2006-01-02 15:04"), e.By, state)
			}
			return tw.Flush()
		},
	}
	var all bool
	rm := &cobra.Command{
		Use:   "rm <name> | --all",
		Short: "Remove a secret; --all removes every secret and the cluster key's ID (a new key comes with the next set)",
		Args: func(_ *cobra.Command, args []string) error {
			if all == (len(args) == 1) || len(args) > 1 {
				return errors.New("give one name, or --all")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			if _, err := localCall(cmd, rpc.AdminOp{Op: "secret_rm", Name: name, All: all}); err != nil {
				return err
			}
			if all {
				fmt.Fprintln(cmd.OutOrStdout(), "removed every secret and the key ID")
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", name)
			}
			return nil
		},
	}
	rm.Flags().BoolVar(&all, "all", false, "remove every secret and the key ID")
	for _, s := range []*cobra.Command{set, ls, rm} {
		addNodeFlags(s)
		c.AddCommand(s)
	}
	return c
}

// readSecret reads one value: typed at a terminal without echo, else the first line of
// stdin. It is never an argument or a flag, which would sit in the shell history and in ps.
func readSecret(cmd *cobra.Command) (string, error) {
	in := cmd.InOrStdin()
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) { //nolint:gosec // a descriptor fits an int
		fmt.Fprint(cmd.ErrOrStderr(), "value: ")
		b, err := term.ReadPassword(int(f.Fd())) //nolint:gosec // a descriptor fits an int
		fmt.Fprintln(cmd.ErrOrStderr())
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if v := strings.TrimRight(line, "\r\n"); v != "" {
		return v, nil
	}
	return "", errors.New("no value on stdin (pipe it in, or run at a terminal to type it)")
}
