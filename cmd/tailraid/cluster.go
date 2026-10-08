package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/node"
	"github.com/zombar/tailraid/internal/rpc"
)

// localClient dials this device's running node over its tailnet RPC address.
func localClient(ctx context.Context, cmd *cobra.Command) (*rpc.Client, string, config.Config, error) {
	cfg, _, err := loadConfig(cmd)
	if err != nil {
		return nil, "", cfg, err
	}
	cid, err := cluster.LoadClusterID(cfg.Node.DataDir)
	if err != nil {
		return nil, "", cfg, fmt.Errorf("this node is not initialised: %w", err)
	}
	_, ident, err := node.Tailnet(ctx, cfg)
	if err != nil {
		return nil, "", cfg, err
	}
	addr := net.JoinHostPort(ident.IP.String(), strconv.Itoa(cfg.Listen.RPC))
	if override, _ := cmd.Flags().GetString("node"); override != "" {
		addr = override
		if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = net.JoinHostPort(addr, strconv.Itoa(cfg.Listen.RPC))
		}
	}
	return rpc.NewClient(cid, nil, nil), addr, cfg, nil
}

func fetchReport(ctx context.Context, cmd *cobra.Command) (admin.Report, *rpc.Client, string, error) {
	c, addr, _, err := localClient(ctx, cmd)
	if err != nil {
		return admin.Report{}, nil, "", err
	}
	raw, err := c.Status(ctx, addr)
	if err != nil {
		return admin.Report{}, nil, "", fmt.Errorf("status from %s (is `tailraid serve` running?): %w", addr, err)
	}
	var rep admin.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		return admin.Report{}, nil, "", err
	}
	return rep, c, addr, nil
}

func addNodeFlags(c *cobra.Command) {
	c.Flags().String("node", "", "RPC address of the node to talk to (default: this device)")
	c.Flags().Bool("json", false, "print JSON")
}

func joinCmd() *cobra.Command {
	var voter bool
	var capacity, name string
	c := &cobra.Command{
		Use:   "join <host> <token>",
		Short: "Join this device to an existing cluster",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, path, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			if name != "" {
				cfg.Node.Name = name
			}
			if capacity != "" {
				cfg.Node.Capacity = capacity
			}
			cfg.Node.Voter = voter
			if err := config.Save(path, cfg); err != nil {
				return err
			}
			resp, err := node.Join(cmd.Context(), cfg, args[0], args[1], voter, version, slog.Default())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "joined cluster %s as node %d (%s); %d members, policy %d+%d\nconfig: %s\nnext: tailraid serve (or tailraid install)\n",
				resp.ClusterID[:8], resp.NodeIdx, cfg.Node.Name, len(resp.Members), resp.Volume.K, resp.Volume.M, path)
			return nil
		},
	}
	c.Flags().BoolVar(&voter, "voter", false, "join as a Raft voter (always-on devices only)")
	c.Flags().StringVar(&capacity, "capacity", "", "shard-byte budget for this node")
	c.Flags().StringVar(&name, "name", "", "node name (default: hostname)")
	return c
}

func statusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Cluster health, members and volume usage",
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep, _, _, err := fetchReport(cmd.Context(), cmd)
			if err != nil {
				return err
			}
			if j, _ := cmd.Flags().GetBool("json"); j {
				return printJSON(cmd.OutOrStdout(), rep)
			}
			printStatus(cmd.OutOrStdout(), rep)
			return nil
		},
	}
	addNodeFlags(c)
	return c
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printStatus(w io.Writer, r admin.Report) {
	fmt.Fprintf(w, "cluster %s  health: %s  leader: %s  voters: %d/%d live\n", r.ClusterID[:8], r.Health, short(r.Leader), r.LiveVoters, r.Voters)
	v := r.Volume
	fmt.Fprintf(w, "volume  %d+%d, chunk %s  files: %d  logical: %s  stored: %s  free: %s  dedup: %.2fx\n",
		v.K, v.M, bytesStr(uint64(v.ChunkSize)), v.Inodes, bytesStr(v.LogicalBytes), bytesStr(v.StoredBytes), bytesStr(v.Free), v.DedupRatio)
	if v.Degraded > 0 || v.Unreadable > 0 {
		fmt.Fprintf(w, "chunks  degraded: %d  unreadable: %d  policy-stale: %d\n", v.Degraded, v.Unreadable, v.StalePolicy)
	}
	printNodes(w, r.Nodes)
	for _, warn := range r.Warnings {
		fmt.Fprintf(w, "! %s\n", warn)
	}
	if r.JoinToken != "" {
		fmt.Fprintf(w, "join: tailraid join <this-device-ip> %s\n", r.JoinToken)
	}
}

func printNodes(w io.Writer, nodes []admin.NodeReport) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "IDX\tNAME\tKIND\tSTATE\tADDR\tVOTER\tLIVE\tSLOTS\tFREE\tVERSION\tFLAGS")
	for i := range nodes {
		n := &nodes[i]
		var flags []string
		if n.Leader {
			flags = append(flags, "leader")
		}
		if n.Self {
			flags = append(flags, "self")
		}
		live := "yes"
		if !n.Live {
			live = "no"
			if n.DeadFor != "" {
				live = "dead " + n.DeadFor
			}
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%v\t%s\t%d\t%s\t%s\t%s\n", n.Idx, n.Name, n.Kind, n.State, n.Addr, n.Voter, live, n.Slots, bytesStr(uint64(max(n.Free, 0))), n.Version, strings.Join(flags, ","))
	}
	_ = tw.Flush()
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func bytesStr(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func nodeCmd() *cobra.Command {
	c := &cobra.Command{Use: "node", Short: "Manage cluster members"}
	ls := &cobra.Command{
		Use: "ls", Short: "List members",
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep, _, _, err := fetchReport(cmd.Context(), cmd)
			if err != nil {
				return err
			}
			if j, _ := cmd.Flags().GetBool("json"); j {
				return printJSON(cmd.OutOrStdout(), rep.Nodes)
			}
			printNodes(cmd.OutOrStdout(), rep.Nodes)
			return nil
		},
	}
	addNodeFlags(ls)
	describe := &cobra.Command{
		Use: "describe <name|idx>", Short: "Show one member in detail", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, _, _, err := fetchReport(cmd.Context(), cmd)
			if err != nil {
				return err
			}
			n, err := findNode(rep, args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), n)
		},
	}
	addNodeFlags(describe)
	op := func(use, short, opName string, build func(cmd *cobra.Command, args []string, op *rpc.MemberOp) error) *cobra.Command {
		oc := &cobra.Command{
			Use: use, Short: short, Args: cobra.MinimumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				rep, c, addr, err := fetchReport(cmd.Context(), cmd)
				if err != nil {
					return err
				}
				n, err := findNode(rep, args[0])
				if err != nil {
					return err
				}
				mo := rpc.MemberOp{Op: opName, NodeIdx: n.Idx}
				if build != nil {
					if err := build(cmd, args, &mo); err != nil {
						return err
					}
				}
				if err := c.Members(cmd.Context(), leaderOr(rep, addr), mo); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %s (node %d)\n", opName, n.Name, n.Idx)
				return nil
			},
		}
		addNodeFlags(oc)
		return oc
	}
	promote := op("promote <node>", "Make a member a Raft voter", "set_voter", func(_ *cobra.Command, _ []string, mo *rpc.MemberOp) error { mo.Voter = true; return nil })
	demote := op("demote <node>", "Make a member a non-voter", "set_voter", nil)
	drain := op("drain <node>", "Stop placing new shards on a member and move its slots away", "set_state", func(_ *cobra.Command, _ []string, mo *rpc.MemberOp) error { mo.State = meta.Draining; return nil })
	activate := op("activate <node>", "Undo drain", "set_state", func(_ *cobra.Command, _ []string, mo *rpc.MemberOp) error { mo.State = meta.Active; return nil })
	setCap := op("set-capacity <node> <bytes>", "Change a member's shard budget (placement weight)", "set_capacity", func(_ *cobra.Command, args []string, mo *rpc.MemberOp) error {
		if len(args) < 2 {
			return errors.New("usage: node set-capacity <node> <bytes>")
		}
		n, err := config.ParseBytes(args[1])
		if err != nil {
			return err
		}
		mo.Capacity = n
		return nil
	})
	remove := op("remove <node>", "Remove a member (refused while it holds shard slots unless --force)", "remove", func(cmd *cobra.Command, _ []string, mo *rpc.MemberOp) error {
		mo.Force, _ = cmd.Flags().GetBool("force")
		return nil
	})
	remove.Flags().Bool("force", false, "remove even if the node still holds shard slots")
	c.AddCommand(ls, describe, promote, demote, drain, activate, setCap, remove)
	return c
}

func leaderOr(rep admin.Report, addr string) string {
	for i := range rep.Nodes {
		if n := &rep.Nodes[i]; n.Leader && n.Addr != "" {
			return net.JoinHostPort(n.Addr, strconv.Itoa(int(n.RPCPort)))
		}
	}
	return addr
}

func findNode(rep admin.Report, key string) (admin.NodeReport, error) {
	for i := range rep.Nodes {
		n := &rep.Nodes[i]
		if n.Name == key || strconv.Itoa(int(n.Idx)) == key || n.ID == key {
			return *n, nil
		}
	}
	return admin.NodeReport{}, fmt.Errorf("no member %q", key)
}

func volumeCmd() *cobra.Command {
	c := &cobra.Command{Use: "volume", Short: "Volume policy and capacity"}
	describe := &cobra.Command{
		Use: "describe", Short: "Policy, usage, dedup and headroom",
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep, _, _, err := fetchReport(cmd.Context(), cmd)
			if err != nil {
				return err
			}
			if j, _ := cmd.Flags().GetBool("json"); j {
				return printJSON(cmd.OutOrStdout(), rep.Volume)
			}
			v := rep.Volume
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "policy        %d+%d (version %d), chunk %s\n", v.K, v.M, v.PolicyVersion, bytesStr(uint64(v.ChunkSize)))
			fmt.Fprintf(w, "inodes        %d\nchunks        %d (%d tombstoned, %d degraded, %d unreadable, %d on an older policy)\n", v.Inodes, v.Chunks, v.Tombstoned, v.Degraded, v.Unreadable, v.StalePolicy)
			fmt.Fprintf(w, "logical       %s\nstored        %s (dedup %.2fx)\n", bytesStr(v.LogicalBytes), bytesStr(v.StoredBytes), v.DedupRatio)
			fmt.Fprintf(w, "capacity      %s raw, %s usable, %s free\n", bytesStr(v.Capacity), bytesStr(v.UsableCapacity), bytesStr(v.Free))
			return nil
		},
	}
	addNodeFlags(describe)
	grow := &cobra.Command{
		Use: "grow", Short: "How to add capacity",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "Capacity grows by joining a device (`tailraid join`) or raising a member's budget (`tailraid node set-capacity <node> <bytes>`). New chunks are placed by capacity weight; existing chunks stay where they are.")
			return nil
		},
	}
	c.AddCommand(describe, grow, setRedundancyCmd())
	return c
}

func tokenCmd() *cobra.Command {
	c := &cobra.Command{
		Use: "token", Short: "Show the join token",
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep, _, _, err := fetchReport(cmd.Context(), cmd)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), rep.JoinToken)
			return nil
		},
	}
	addNodeFlags(c)
	return c
}

func adminCall(cmd *cobra.Command, op rpc.AdminOp) (json.RawMessage, error) {
	rep, c, addr, err := fetchReport(cmd.Context(), cmd)
	if err != nil {
		return nil, err
	}
	return c.Admin(cmd.Context(), leaderOr(rep, addr), op)
}

func printRaw(w io.Writer, raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		_, err = w.Write(raw)
		return err
	}
	return printJSON(w, v)
}

func checkCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "check",
		Short: "Replication health: degraded and unreadable chunks, last repair pass (exit 1 if issues)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			scrub, _ := cmd.Flags().GetBool("scrub")
			files, _ := cmd.Flags().GetBool("files")
			raw, err := adminCall(cmd, rpc.AdminOp{Op: "check", Scrub: scrub, Files: files})
			if err != nil {
				return err
			}
			if err := printRaw(cmd.OutOrStdout(), raw); err != nil {
				return err
			}
			var rep struct {
				OK bool `json:"ok"`
			}
			_ = json.Unmarshal(raw, &rep)
			if !rep.OK {
				return errors.New("check found issues")
			}
			return nil
		},
	}
	addNodeFlags(c)
	c.Flags().Bool("scrub", false, "verify every shard of every chunk (slow)")
	c.Flags().Bool("files", false, "list files touching degraded or unreadable chunks")
	return c
}

func repairCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "repair",
		Short: "Run a repair pass on the leader now and print its statistics",
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := adminCall(cmd, rpc.AdminOp{Op: "repair_now"})
			if err != nil {
				return err
			}
			return printRaw(cmd.OutOrStdout(), raw)
		},
	}
	addNodeFlags(c)
	return c
}

func gcCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "gc",
		Short: "Collect tombstoned chunks older than the grace period now",
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := adminCall(cmd, rpc.AdminOp{Op: "gc_now"})
			if err != nil {
				return err
			}
			return printRaw(cmd.OutOrStdout(), raw)
		},
	}
	addNodeFlags(c)
	return c
}

func fsckCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "fsck",
		Short: "Check metadata invariants and count orphan shards on the target node",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, cl, addr, err := fetchReport(cmd.Context(), cmd)
			if err != nil {
				return err
			}
			raw, err := cl.Admin(cmd.Context(), addr, rpc.AdminOp{Op: "fsck"})
			if err != nil {
				return err
			}
			if err := printRaw(cmd.OutOrStdout(), raw); err != nil {
				return err
			}
			var rep struct {
				OK bool `json:"ok"`
			}
			_ = json.Unmarshal(raw, &rep)
			if !rep.OK {
				return errors.New("fsck found violations")
			}
			return nil
		},
	}
	addNodeFlags(c)
	return c
}

func setRedundancyCmd() *cobra.Command {
	var k, m int
	c := &cobra.Command{
		Use:   "set-redundancy",
		Short: "Change the volume's k+m; existing chunks migrate in the background",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if k < 1 || m < 0 {
				return errors.New("--k and --m are required")
			}
			raw, err := adminCall(cmd, rpc.AdminOp{Op: "set_policy", K: uint8(k), M: uint8(m)})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "policy set to %d+%d; existing chunks migrate on the next repair passes (watch `tailraid volume describe`)\n", k, m)
			return printRaw(cmd.OutOrStdout(), raw)
		},
	}
	addNodeFlags(c)
	c.Flags().IntVar(&k, "k", 0, "data shards")
	c.Flags().IntVar(&m, "m", -1, "parity shards")
	return c
}
