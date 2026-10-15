package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"runtime"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/node"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/smbgw"
	"github.com/zombar/tailraid/internal/users"
)

// The users, groups and permissions commands (docs/21 §9, docs/12 §12). Every verb but
// `user create`, `user key add` and `user bootstrap` is a signed request: localClient
// attaches the signature of the identity this person holds for the cluster.

// userSigner signs the CLI's mutating requests with the identity in the person's keychain
// (docs/21 §2, I-344); with no identity the requests go unsigned, which a cluster with
// users refuses with the hint to create one.
func userSigner(ctx context.Context, cid [16]byte) func(addr, method, path string, body []byte) (string, error) {
	var (
		once sync.Once // the keychain is read on the first mutating request, never for a read-only command
		id   users.Identity
		ok   bool
		err  error
		mu   sync.Mutex
	)
	nodeIDs := map[string]string{}
	return func(addr, method, path string, body []byte) (string, error) {
		once.Do(func() { id, ok, err = users.Load(users.UserKeychain(), cid) })
		if err != nil {
			return "", err
		}
		if !ok {
			return "", nil
		}
		mu.Lock()
		nodeID, known := nodeIDs[addr]
		mu.Unlock()
		if !known {
			h, err := rpc.NewClient(cid, nil).Health(ctx, addr)
			if err != nil {
				return "", fmt.Errorf("the node at %s: %w", addr, err)
			}
			nodeID = h.NodeID
			mu.Lock()
			nodeIDs[addr] = nodeID
			mu.Unlock()
		}
		nonce, err := users.NewNonce()
		if err != nil {
			return "", err
		}
		return users.Sign(id, cid, nodeID, method, path, body, time.Now().Unix(), nonce).String(), nil
	}
}

// osUserName is the name of the OS account running the CLI.
func osUserName() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

func userCmd() *cobra.Command {
	c := &cobra.Command{Use: "user", Short: "People of the volume: identities with a key per device, from an admin's invite (docs/21)"}
	var admin bool
	invite := &cobra.Command{
		Use:   "invite <name>",
		Short: "Mint a one-time code (24 h) that creates the user; admins only",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := localCall(cmd, rpc.AdminOp{Op: "user_invite", User: args[0], Admin: admin})
			if err != nil {
				return err
			}
			return printInvite(cmd, raw, args[0])
		},
	}
	invite.Flags().BoolVar(&admin, "admin", false, "the new user joins admins")
	var label string
	create := &cobra.Command{
		Use:   "create <name> <code>",
		Short: "Create your identity on this device from an invite: the key stays in your keychain, the public half is registered",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Getenv("SUDO_USER") != "" {
				return errors.New("run user create as yourself, not with sudo: the key goes into your own keychain")
			}
			c, addr, _, err := localClient(cmd)
			if err != nil {
				return err
			}
			cid := clusterOf(c)
			kc := users.UserKeychain()
			id, err := users.Create(kc, cid, args[0])
			if err != nil {
				return err
			}
			pub := id.Pub()
			raw, err := c.Admin(cmd.Context(), addr, rpc.AdminOp{Op: "user_create", User: args[0], Code: args[1], Key: hex.EncodeToString(pub[:]), Label: labelOr(label)})
			if err != nil {
				_ = users.Forget(kc, cid) // the identity is for the row the invite makes; none, none
				return err
			}
			return bindAndReport(cmd, raw, args[0])
		},
	}
	create.Flags().StringVar(&label, "label", "", "the key's label (default: this host's name)")
	key := &cobra.Command{Use: "key", Short: "Your keys: one per device"}
	keyInvite := &cobra.Command{
		Use:   "invite [<name>]",
		Short: "Mint a one-time code that adds a key to your user (an admin: to any user)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			op := rpc.AdminOp{Op: "user_key_invite"}
			if len(args) == 1 {
				op.User = args[0]
			}
			raw, err := localCall(cmd, op)
			if err != nil {
				return err
			}
			return printInvite(cmd, raw, "")
		},
	}
	keyAdd := &cobra.Command{
		Use:   "add <code>",
		Short: "On a new device: make a key here and add it to your user with the code",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Getenv("SUDO_USER") != "" {
				return errors.New("run user key add as yourself, not with sudo")
			}
			c, addr, _, err := localClient(cmd)
			if err != nil {
				return err
			}
			cid := clusterOf(c)
			kc := users.UserKeychain()
			if _, ok, err := users.Load(kc, cid); err != nil {
				return err
			} else if ok {
				return users.ErrHasIdent
			}
			id, err := users.NewIdentity("pending")
			if err != nil {
				return err
			}
			pub := id.Pub()
			raw, err := c.Admin(cmd.Context(), addr, rpc.AdminOp{Op: "user_key_add", Code: args[0], Key: hex.EncodeToString(pub[:]), Label: labelOr(label)})
			if err != nil {
				return err
			}
			var r struct {
				User string `json:"user"`
			}
			if err := json.Unmarshal(raw, &r); err != nil || r.User == "" {
				return fmt.Errorf("user key add reply: %w", err)
			}
			id.Name = r.User
			if err := users.Keep(kc, cid, id); err != nil {
				return err
			}
			return bindAndReport(cmd, raw, r.User)
		},
	}
	keyAdd.Flags().StringVar(&label, "label", "", "the key's label (default: this host's name)")
	keyRm := &cobra.Command{
		Use:   "rm <name> <key-id>",
		Short: "Remove one of a user's keys (your own, or any as an admin); its browser sessions end with it",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return callAndSay(cmd, rpc.AdminOp{Op: "user_key_rm", User: args[0], Key: args[1]}, "removed key %s of %s", args[1], args[0])
		},
	}
	key.AddCommand(keyInvite, keyAdd, keyRm)
	ls := &cobra.Command{
		Use:   "ls [<name>]",
		Short: "List the users; with a name, that user's keys, groups and state",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			op := rpc.AdminOp{Op: "user_ls"}
			if len(args) == 1 {
				op.User = args[0]
			}
			raw, err := localCall(cmd, op)
			if err != nil {
				return err
			}
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return printRaw(cmd.OutOrStdout(), raw)
			}
			var rows []node.UserRow
			if err := json.Unmarshal(raw, &rows); err != nil {
				return fmt.Errorf("user ls reply: %w", err)
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no users (tailraid user bootstrap on a voter mints the first invite)")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATE\tKEYS\tGROUPS\tCREATED\tBY")
			for _, r := range rows {
				keys := make([]string, 0, len(r.Keys))
				for _, k := range r.Keys {
					if len(args) == 1 {
						keys = append(keys, k.ID+" ("+k.Label+")")
					} else {
						keys = append(keys, k.Label)
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.State, strings.Join(keys, ", "), strings.Join(r.Groups, ", "), time.Unix(0, r.Created).Local().Format("2006-01-02"), r.By)
			}
			return tw.Flush()
		},
	}
	whoami := &cobra.Command{
		Use:   "whoami",
		Short: "The user this device's identity signs for",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := localCall(cmd, rpc.AdminOp{Op: "user_whoami"})
			if err != nil {
				return err
			}
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return printRaw(cmd.OutOrStdout(), raw)
			}
			var r struct {
				User  string `json:"user"`
				Admin bool   `json:"admin"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
			role := "user"
			if r.Admin {
				role = "admin"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s (%s)\n", r.User, role)
			return nil
		},
	}
	state := func(verb, op, past string) *cobra.Command {
		return &cobra.Command{
			Use:   verb + " <name>",
			Short: strings.ToUpper(verb[:1]) + verb[1:] + " a user; admins only",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if _, err := localCall(cmd, rpc.AdminOp{Op: op, User: args[0]}); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", past, args[0])
				return nil
			},
		}
	}
	var uid int
	var smb string
	bind := &cobra.Command{
		Use:   "bind <name>",
		Short: "Map a local credential of this device (your uid by default, --uid n, --smb account) to a user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cred := users.UIDCredential(uid)
			if cmd.Flags().Changed("smb") {
				cred = users.SMBCredential(smb)
			} else if !cmd.Flags().Changed("uid") {
				cred = users.UIDCredential(users.CallerUID())
			}
			if _, err := localCall(cmd, rpc.AdminOp{Op: "user_bind", Credential: cred, User: args[0]}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "bound %s to %s on this device\n", cred, args[0])
			return nil
		},
	}
	bind.Flags().IntVar(&uid, "uid", 0, "the uid to bind")
	bind.Flags().StringVar(&smb, "smb", "", "the SMB account to bind")
	unbind := &cobra.Command{
		Use:   "unbind",
		Short: "Forget a local credential's binding on this device (your uid by default)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cred := users.UIDCredential(uid)
			if cmd.Flags().Changed("smb") {
				cred = users.SMBCredential(smb)
			} else if !cmd.Flags().Changed("uid") {
				cred = users.UIDCredential(users.CallerUID())
			}
			if _, err := localCall(cmd, rpc.AdminOp{Op: "user_unbind", Credential: cred}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "unbound %s on this device\n", cred)
			return nil
		},
	}
	unbind.Flags().IntVar(&uid, "uid", 0, "the uid to unbind")
	unbind.Flags().StringVar(&smb, "smb", "", "the SMB account to unbind")
	var bootName string
	bootstrap := &cobra.Command{
		Use:   "bootstrap",
		Short: "As the daemon's user on a voter: mint an admin invite (the first admin, or a lockout)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, _, cfg, err := localClient(cmd)
			if err != nil {
				return err
			}
			tok, err := users.WriteBootstrap(cfg.Node.DataDir)
			if err != nil {
				return fmt.Errorf("user bootstrap runs as the daemon's user (it writes the data directory): %w", err)
			}
			name := bootName
			if name == "" {
				name = users.DefaultName(osUserName())
			}
			raw, err := localCall(cmd, rpc.AdminOp{Op: "user_bootstrap", Value: tok, User: name})
			if err != nil {
				return err
			}
			return printInvite(cmd, raw, name)
		},
	}
	bootstrap.Flags().StringVar(&bootName, "name", "", "the admin's name (default: yours)")
	for _, s := range []*cobra.Command{invite, create, key, ls, whoami, state("disable", "user_disable", "disabled"), state("enable", "user_enable", "enabled"), state("rm", "user_rm", "removed"), bind, unbind, bootstrap} {
		addNodeFlags(s)
		c.AddCommand(s)
	}
	for _, s := range []*cobra.Command{keyInvite, keyAdd, keyRm} {
		addNodeFlags(s)
	}
	return c
}

// clusterOf is the client's cluster ID.
func clusterOf(c *rpc.Client) [16]byte {
	var cid [16]byte
	b, _ := hex.DecodeString(c.ClusterID())
	copy(cid[:], b)
	return cid
}

func labelOr(label string) string {
	if label != "" {
		return label
	}
	h, err := os.Hostname()
	if err != nil {
		return "device"
	}
	return strings.ToLower(strings.SplitN(h, ".", 2)[0])
}

// printInvite prints an invite's one-time code as the command to run.
func printInvite(cmd *cobra.Command, raw json.RawMessage, name string) error {
	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		return printRaw(cmd.OutOrStdout(), raw)
	}
	var r node.InviteReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("invite reply: %w", err)
	}
	until := time.Unix(0, r.Expires).Local().Format("2006-01-02 15:04")
	if r.User != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "key invite for %s, valid until %s; on the new device, as %s:\n  tailraid user key add %s\n", r.User, until, r.User, r.Code)
		return nil
	}
	if r.Name == "" {
		r.Name = name
	}
	what := "user"
	if r.Admin {
		what = "admin"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s invite for %s, valid until %s; on their device, as themselves (not sudo):\n  tailraid user create %s %s\n", what, r.Name, until, r.Name, r.Code)
	return nil
}

// bindAndReport binds this person's own credential on the device (I-350) and says what
// was made.
func bindAndReport(cmd *cobra.Command, raw json.RawMessage, name string) error {
	cred := deviceCredential()
	if _, err := localCall(cmd, rpc.AdminOp{Op: "user_bind", Credential: cred, User: name}); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "identity made, but this device's mount is not bound to it: %v (tailraid user bind %s)\n", err, name)
	}
	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		return printRaw(cmd.OutOrStdout(), raw)
	}
	var r struct {
		User string `json:"user"`
		Key  string `json:"key"`
	}
	_ = json.Unmarshal(raw, &r)
	fmt.Fprintf(cmd.OutOrStdout(), "you are %s on this device (key %s, bound to %s)\n", r.User, r.Key, cred)
	return nil
}

// deviceCredential is the binding key this person's IO through the device's mount
// presents: their uid over NFS, the share's account over SMB on Windows (docs/21 §5).
func deviceCredential() string {
	if runtime.GOOS == "windows" {
		return users.SMBCredential(smbgw.User)
	}
	return users.UIDCredential(users.CallerUID())
}

func groupCmd() *cobra.Command {
	c := &cobra.Command{Use: "group", Short: "Groups of users: the roles a grant names as @group (docs/21); admins only"}
	simple := func(use, short, op string, n int, fill func(args []string) rpc.AdminOp, done string) *cobra.Command {
		return &cobra.Command{
			Use: use, Short: short, Args: cobra.ExactArgs(n),
			RunE: func(cmd *cobra.Command, args []string) error {
				o := fill(args)
				o.Op = op
				if _, err := localCall(cmd, o); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), done+"\n", anySlice(args)...)
				return nil
			},
		}
	}
	ls := &cobra.Command{
		Use: "ls", Short: "List the groups and their members", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := localCall(cmd, rpc.AdminOp{Op: "group_ls"})
			if err != nil {
				return err
			}
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return printRaw(cmd.OutOrStdout(), raw)
			}
			var rows []node.GroupRow
			if err := json.Unmarshal(raw, &rows); err != nil {
				return fmt.Errorf("group ls reply: %w", err)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "GROUP\tMEMBERS")
			for _, r := range rows {
				members := strings.Join(r.Members, ", ")
				if r.Name == "everyone" {
					members = "(every active user)"
				}
				fmt.Fprintf(tw, "%s\t%s\n", r.Name, members)
			}
			return tw.Flush()
		},
	}
	for _, s := range []*cobra.Command{
		simple("create <group>", "Create a group", "group_create", 1, func(a []string) rpc.AdminOp { return rpc.AdminOp{Group: a[0]} }, "created group %s"),
		simple("rm <group>", "Remove a group and its grants", "group_rm", 1, func(a []string) rpc.AdminOp { return rpc.AdminOp{Group: a[0]} }, "removed group %s"),
		simple("add <group> <user>", "Add a user to a group", "group_add", 2, func(a []string) rpc.AdminOp { return rpc.AdminOp{Group: a[0], User: a[1]} }, "added %[2]s to %[1]s"),
		simple("remove <group> <user>", "Take a user out of a group", "group_remove", 2, func(a []string) rpc.AdminOp { return rpc.AdminOp{Group: a[0], User: a[1]} }, "removed %[2]s from %[1]s"),
		ls,
	} {
		addNodeFlags(s)
		c.AddCommand(s)
	}
	return c
}

// callAndSay sends an admin op to this device's node and prints one line on success.
func callAndSay(cmd *cobra.Command, op rpc.AdminOp, format string, args ...any) error {
	if _, err := localCall(cmd, op); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), format+"\n", args...)
	return nil
}

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func grantCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "grant <path> <user|@group> read|write|admin",
		Short: "Give a user or a group a role on a folder and everything under it (docs/21 §3); needs admin there",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := localCall(cmd, rpc.AdminOp{Op: "grant", Path: args[0], User: args[1], Role: args[2]}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "granted %s %s on %s\n", args[1], args[2], args[0])
			return nil
		},
	}
	addNodeFlags(c)
	return c
}

func revokeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "revoke <path> <user|@group>",
		Short: "Clear a grant on a folder; needs admin there",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return callAndSay(cmd, rpc.AdminOp{Op: "revoke", Path: args[0], User: args[1]}, "revoked %s on %s", args[1], args[0])
		},
	}
	addNodeFlags(c)
	return c
}

func accessCmd() *cobra.Command {
	c := &cobra.Command{Use: "access", Short: "The access review: every grant with its subject, role, grantor and time (docs/21 §8)"}
	ls := &cobra.Command{
		Use:   "ls [path]",
		Short: "List the grants, under a folder when given",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			op := rpc.AdminOp{Op: "access_ls"}
			if len(args) == 1 {
				op.Path = args[0]
			}
			raw, err := localCall(cmd, op)
			if err != nil {
				return err
			}
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return printRaw(cmd.OutOrStdout(), raw)
			}
			var rows []node.GrantRow
			if err := json.Unmarshal(raw, &rows); err != nil {
				return fmt.Errorf("access ls reply: %w", err)
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no grants: nothing is shared until it is (admins see everything)")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "PATH\tSUBJECT\tROLE\tBY\tAT")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Path, r.Subject, r.Role, r.By, time.Unix(0, r.At).Local().Format("2006-01-02 15:04"))
			}
			return tw.Flush()
		},
	}
	addNodeFlags(ls)
	c.AddCommand(ls)
	return c
}
