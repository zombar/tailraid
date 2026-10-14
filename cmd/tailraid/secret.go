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
	"github.com/zombar/tailraid/internal/users"
)

func secretCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "secret",
		Short: "The cluster's secrets store: values sealed in the metadata, the key in each device's keychain (docs/19)",
	}
	var system bool
	set := &cobra.Command{
		Use:   "set <name>",
		Short: "Store a secret of your own (--system: one of the cluster's, admins only); the value is read from stdin, never from an argument",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			value, err := readSecret(cmd)
			if err != nil {
				return err
			}
			name, err := secretName(cmd, args[0], system)
			if err != nil {
				return err
			}
			if _, err := localCall(cmd, rpc.AdminOp{Op: "secret_set", Name: name, Value: value}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stored %s\n", name)
			return nil
		},
	}
	set.Flags().BoolVar(&system, "system", false, "a secret of the cluster (s3/<node>/…, tmdb), not your own")
	get := &cobra.Command{
		Use:   "get <name>",
		Short: "Print one of your own secrets (never a system secret: those are opened by the node that uses them)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := secretName(cmd, args[0], false)
			if err != nil {
				return err
			}
			v, err := storedSecret(cmd, name)
			if err != nil {
				return err
			}
			if v == "" {
				return fmt.Errorf("no secret %s", name)
			}
			fmt.Fprintln(cmd.OutOrStdout(), v)
			return nil
		},
	}
	var lsSystem bool
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List your secrets (--system: the cluster's, admins only): names, when and by whom each was set, and whether this node can open it; never a value",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := localCall(cmd, rpc.AdminOp{Op: "secret_ls", All: lsSystem})
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
				by := fmt.Sprintf("node %d", e.By)
				if e.ByUser != 0 {
					by = fmt.Sprintf("user %d", e.ByUser)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Name, time.Unix(0, e.At).Local().Format("2006-01-02 15:04"), by, state)
			}
			return tw.Flush()
		},
	}
	ls.Flags().BoolVar(&lsSystem, "system", false, "the cluster's secrets, not your own (admins)")
	var all, rmSystem bool
	rm := &cobra.Command{
		Use:   "rm <name> | --all",
		Short: "Remove one of your secrets (--system: one of the cluster's); --all removes every secret and the cluster key's ID",
		Args: func(_ *cobra.Command, args []string) error {
			if all == (len(args) == 1) || len(args) > 1 {
				return errors.New("give one name, or --all")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				var err error
				if name, err = secretName(cmd, args[0], rmSystem); err != nil {
					return err
				}
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
	rm.Flags().BoolVar(&rmSystem, "system", false, "a secret of the cluster, not your own")
	for _, s := range []*cobra.Command{set, get, ls, rm} {
		addNodeFlags(s)
		c.AddCommand(s)
	}
	return c
}

// secretName is the name a secret op addresses (docs/21 §7): with --system, or on a
// cluster without users, the name as given; otherwise `user/<me>/<name>`, the caller's
// own name space. A name already in that form is kept.
func secretName(cmd *cobra.Command, name string, system bool) (string, error) {
	if system || strings.HasPrefix(name, "user/") {
		return name, nil
	}
	c, _, _, err := localClient(cmd.Context(), cmd)
	if err != nil {
		return "", err
	}
	id, ok, err := users.Load(users.UserKeychain(), clusterOf(c))
	if err != nil {
		return "", err
	}
	if !ok {
		return name, nil // no identity here: a cluster without users, or a system secret by the operator
	}
	return "user/" + id.Name + "/" + name, nil
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

// storedSecret is the value of name through this device's node, "" when the store has none.
func storedSecret(cmd *cobra.Command, name string) (string, error) {
	raw, err := localCall(cmd, rpc.AdminOp{Op: "secret_get", Name: name})
	if err != nil {
		if strings.Contains(err.Error(), secrets.ErrNotFound.Error()) {
			return "", nil
		}
		return "", err
	}
	var v struct {
		Value string `json:"value"`
	}
	err = json.Unmarshal(raw, &v)
	return v.Value, err
}
