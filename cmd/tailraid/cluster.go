package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/node"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/svc"
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
	return rpc.NewClient(cid, nil), addr, cfg, nil
}

func fetchReport(ctx context.Context, cmd *cobra.Command) (admin.Report, *rpc.Client, string, error) {
	c, addr, _, err := localClient(ctx, cmd)
	if err != nil {
		return admin.Report{}, nil, "", err
	}
	rep, err := admin.FetchStatus(ctx, c, addr)
	if err != nil {
		return admin.Report{}, nil, "", fmt.Errorf("status from %s (is `tailraid serve` running?): %w", addr, err)
	}
	return rep, c, addr, nil
}

func addNodeFlags(c *cobra.Command) {
	c.Flags().String("node", "", "RPC address of the node to talk to (default: this device)")
	c.Flags().Bool("json", false, "print JSON")
}

func joinCmd() *cobra.Command {
	var voter bool
	var name string
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
			cfg.Node.Voter = voter
			cfg.EnsureSMBPassword() // the share password, when this device serves SMB (docs/15 §6)
			if err := config.Save(path, cfg); err != nil {
				return err
			}
			resp, err := node.Join(cmd.Context(), cfg, args[0], args[1], voter, version, slog.Default())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "joined cluster %s as node %d (%s); %d members, policy %d+%d\nconfig: %s\n",
				resp.ClusterID[:8], resp.NodeIdx, cfg.Node.Name, len(resp.Members), resp.Volume.K, resp.Volume.M, path)
			printDisks(cmd.OutOrStdout(), cfg, resp.Disks)
			fmt.Fprintln(cmd.OutOrStdout(), "next: tailraid serve (or tailraid install)")
			return nil
		},
	}
	c.Flags().BoolVar(&voter, "voter", false, "join as a Raft voter (always-on devices only)")
	c.Flags().StringVar(&name, "name", "", "node name (default: hostname)")
	return c
}

// printDisks lists the disks init or join registered, one line each with its budget, or
// the hint for a device that stores nothing (docs/14 §2, I-121).
func printDisks(w io.Writer, cfg config.Config, disks []meta.Node) {
	if len(disks) == 0 {
		fmt.Fprintln(w, node.NoDisksHint)
		return
	}
	for i := range disks {
		fmt.Fprintf(w, "disk %s: %s (budget %s)\n", disks[i].Name, cfg.Node.Disks[i].Path, bytesStr(disks[i].Capacity))
	}
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
				if err := printJSON(cmd.OutOrStdout(), rep); err != nil {
					return err
				}
			} else {
				printStatus(cmd.OutOrStdout(), rep)
			}
			if !rep.Ready { // for supervisors and scripts: exit 1 until the node can serve (I-125)
				return errors.New("node is not ready: no leader, or the log is still being replayed")
			}
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
	fmt.Fprintf(w, "        survives %s\n", survives(v.ToleratesDisks, v.ToleratesHosts))
	if v.Degraded > 0 || v.RepairPending > 0 || v.Unreadable > 0 || v.DomainSkew > 0 || v.DiskSkew > 0 {
		fmt.Fprintf(w, "chunks  degraded: %d  repair-pending: %d  unreadable: %d  policy-stale: %d  host-skew: %d  disk-skew: %d\n", v.Degraded, v.RepairPending, v.Unreadable, v.StalePolicy, v.DomainSkew, v.DiskSkew)
	}
	if r.Repair != nil && r.Repair.InProgress != nil {
		p := r.Repair.InProgress
		fmt.Fprintf(w, "repair  pass %d running: %d/%d scanned, %d in flight of %d workers, %s moved, started %s\n",
			p.Pass, p.Scanned, p.Total, p.InFlight, p.Workers, bytesStr(uint64(max(p.BytesMoved, 0))), p.Started.Local().Format("15:04:05"))
	}
	if r.Repair != nil {
		if l := homeLine(r.Repair.Home); l != "" {
			fmt.Fprintln(w, l)
		}
	}
	if r.WebURL != "" {
		fmt.Fprintf(w, "web     %s\n", r.WebURL)
	}
	for i := range r.Mirrors { // every mirror of the cluster, from the probes (docs/12 §11)
		m := &r.Mirrors[i]
		state := "offline (" + m.Reason + ")"
		if m.Online {
			state = fmt.Sprintf("online · %s used, %s free", bytesStr(uint64(max(m.Used, 0))), bytesStr(m.Free))
		}
		newest := "no snapshot yet"
		if m.Newest != 0 {
			newest = "newest " + time.Unix(0, m.Newest).Local().Format("2006-01-02 15:04")
		}
		if m.Syncing != nil {
			newest += fmt.Sprintf(" · syncing %s %d %%", m.Syncing.Snapshot, m.Syncing.Percent())
		}
		fmt.Fprintf(w, "mirror  %s  %s · %s\n", cmp.Or(m.Name, m.Path), state, newest)
	}
	if v.Snapshots > 0 {
		fmt.Fprintf(w, "        %d snapshots on the mirrors\n", v.Snapshots)
	}
	if s := r.Search; s != nil && (s.Docs > 0 || s.Pending > 0 || s.Index != "") {
		line := fmt.Sprintf("index   %d files, %d segments, %s, catalogue %d", s.Docs, s.Segments, bytesStr(uint64(max(s.Bytes, 0))), s.Catalog)
		if s.Pending > 0 {
			line += fmt.Sprintf(", %d pending", s.Pending)
		}
		if s.Capped {
			line += ", capped: contents past search.max_bytes are not indexed"
		}
		if s.Index != "" {
			line += ", " + s.Index
		}
		fmt.Fprintln(w, line)
	}
	printNodes(w, r.Nodes, false)
	for _, warn := range r.Warnings {
		fmt.Fprintf(w, "! %s\n", warn)
	}
	if r.JoinToken != "" {
		fmt.Fprintf(w, "join: tailraid join <this-device-ip> %s\n", r.JoinToken)
	}
}

// hostOrder lists the nodes with each device's disks right after it (docs/14 §8).
func hostOrder(nodes []admin.NodeReport) []admin.NodeReport {
	out := make([]admin.NodeReport, 0, len(nodes))
	for i := range nodes {
		if nodes[i].Kind == "disk" {
			continue
		}
		out = append(out, nodes[i])
		for j := range nodes {
			if nodes[j].Kind == "disk" && nodes[j].Host == nodes[i].Idx {
				out = append(out, nodes[j])
			}
		}
	}
	for i := range nodes { // a disk whose host is missing from the list still shows
		if nodes[i].Kind == "disk" && !slices.ContainsFunc(out, func(n admin.NodeReport) bool { return n.Idx == nodes[i].Idx }) {
			out = append(out, nodes[i])
		}
	}
	return out
}

// printNodes prints the member table. A Removed row stays in the table for good (an index
// is never reused, docs/02), so it is left out unless all is set (I-132).
func printNodes(w io.Writer, nodes []admin.NodeReport, all bool) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "IDX\tNAME\tKIND\tSTATE\tADDR\tVOTER\tLIVE\tSLOTS\tFREE\tVERSION\tFLAGS")
	if !all {
		nodes = slices.DeleteFunc(slices.Clone(nodes), func(n admin.NodeReport) bool { return n.State == "removed" })
	}
	nodes = hostOrder(nodes)
	for i := range nodes {
		n := &nodes[i]
		name := n.Name
		if n.Kind == "disk" {
			name = "  " + name // indented under its host
		}
		var flags []string
		if n.Leader {
			flags = append(flags, "leader")
		}
		if n.Self {
			flags = append(flags, "self")
		}
		if n.Diskless {
			flags = append(flags, "no disks") // a gateway-only host (docs/14 §9)
		}
		if len(n.SharedWith) > 0 {
			flags = append(flags, "shares disk with "+strings.Join(n.SharedWith, ", "))
		}
		live := "yes"
		if !n.Live {
			live = "no"
			if n.DeadFor != "" {
				live = "dead " + n.DeadFor
			}
		}
		free := "-" // a device stores nothing; its disks have the free space
		addr := n.Addr
		switch {
		case n.S3 != nil: // a bucket reports no free space, and its address is the bucket
			addr = path.Join(n.S3.Bucket, n.S3.Prefix)
		case n.Kind != "device":
			free = bytesStr(uint64(max(n.Free, 0)))
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%v\t%s\t%d\t%s\t%s\t%s\n", n.Idx, name, n.Kind, n.State, addr, n.Voter, live, n.Slots, free, n.Version, strings.Join(flags, ", "))
	}
	_ = tw.Flush()
}

// activeHosts counts the Active devices of the node table.
func activeHosts(nodes []admin.NodeReport) int {
	n := 0
	for i := range nodes {
		if nodes[i].Kind == "device" && nodes[i].State == "active" {
			n++
		}
	}
	return n
}

// survives prints a policy's tolerance: "2 disks, 1 host" (docs/14 §6).
func survives(disks, hosts int) string {
	plural := func(n int, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %ss", n, one)
	}
	return plural(disks, "disk") + ", " + plural(hosts, "host")
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
			all, _ := cmd.Flags().GetBool("all")
			printNodes(cmd.OutOrStdout(), rep.Nodes, all)
			return nil
		},
	}
	ls.Flags().Bool("all", false, "include removed members (their index is never reused)")
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
				if opName == "set_voter" {
					fmt.Fprintln(cmd.OutOrStdout(), "the member table is the vote's record; the device's config file follows it while it serves (node.voter)")
				}
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
	remove := op("remove <node>", "Remove a member (refused while it holds shard slots unless --force)", "remove", func(cmd *cobra.Command, _ []string, mo *rpc.MemberOp) error {
		mo.Force, _ = cmd.Flags().GetBool("force")
		return nil
	})
	remove.Flags().Bool("force", false, "remove even if the node still holds shard slots")
	c.AddCommand(ls, describe, promote, demote, drain, activate, remove, addDiskCmd(), addS3Cmd())
	return c
}

// addDiskCmd is `node add-disk <path>`: mark an empty directory as a disk of this device,
// save it in the config, register it through the leader and have the running node open
// it (docs/14 §2).
func addDiskCmd() *cobra.Command {
	var capacity string
	c := &cobra.Command{
		Use: "add-disk <path>", Short: "Add a disk of this device to the volume as a placement target of its own", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, path, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			disk := config.Disk{Path: filepath.Clean(args[0]), Capacity: capacity}
			next := cfg
			next.Node.Disks = append(append([]config.Disk(nil), cfg.Node.Disks...), disk)
			if err := next.Validate(); err != nil {
				return err
			}
			rep, c, addr, err := fetchReport(cmd.Context(), cmd)
			if err != nil {
				return err
			}
			var host *admin.NodeReport
			n := 1
			for i := range rep.Nodes {
				if rep.Nodes[i].Self && rep.Nodes[i].Kind == "device" {
					host = &rep.Nodes[i]
				}
			}
			if host == nil {
				return errors.New("this device is not in the node table")
			}
			for i := range rep.Nodes {
				if rep.Nodes[i].Kind == "disk" && rep.Nodes[i].Host == host.Idx {
					n++
				}
			}
			cid, err := cluster.LoadClusterID(cfg.Node.DataDir)
			if err != nil {
				return err
			}
			id, err := node.PrepareDisk(disk.Path, cid, host.ID)
			if err != nil {
				return err
			}
			budget, err := node.DiskBudget(disk)
			if err != nil {
				return err
			}
			if err := config.Save(path, next); err != nil {
				return err
			}
			name := node.DiskName(host.Name, n)
			if err := c.Members(cmd.Context(), leaderOr(rep, addr), rpc.MemberOp{Op: "add_disk", NodeIdx: host.Idx, DiskID: id, Name: name, Capacity: budget}); err != nil {
				return fmt.Errorf("register %s (the marker and the config entry stay; re-run to retry): %w", name, err)
			}
			// a systemd service may write only what its unit names: rewrite the unit with the
			// new disk and restart, or the disk opens read-only for it (I-147)
			if runtime.GOOS == "linux" && svc.UnitInstalled() {
				if err := refreshUnit(cmd); err != nil {
					return fmt.Errorf("registered %s, but the service unit was not updated (run `tailraid install`, then restart the service): %w", name, err)
				}
			}
			raw, err := openDiskWhenUp(cmd, c, addr, rpc.AdminOp{Op: "open_disk", Path: disk.Path, Capacity: capacity})
			if err != nil {
				return err
			}
			var st struct {
				State string `json:"state"`
			}
			_ = json.Unmarshal(raw, &st)
			if rep, _, _, err := fetchReport(cmd.Context(), cmd); err == nil { // the leader settles the name
				for i := range rep.Nodes {
					if rep.Nodes[i].ID == id {
						name = rep.Nodes[i].Name
					}
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added disk %s at %s (budget %s): %s\n", name, disk.Path, bytesStr(budget), st.State)
			return nil
		},
	}
	c.Flags().StringVar(&capacity, "capacity", "", `pin the disk's budget ("500G"); default: derived from its filesystem`)
	addNodeFlags(c)
	return c
}

// leaderOr is the leader's RPC address, or addr when the report names no leader.
func leaderOr(rep admin.Report, addr string) string {
	if l := rep.LeaderRPC(); l != "" {
		return l
	}
	return addr
}

// findNode resolves an index, ID or name. A device reset and joined again leaves a
// removed row under the same name, so a name prefers the row that is not removed (I-136).
func findNode(rep admin.Report, key string) (admin.NodeReport, error) {
	var removed *admin.NodeReport
	for i := range rep.Nodes {
		n := &rep.Nodes[i]
		if strconv.Itoa(int(n.Idx)) == key || n.ID == key {
			return *n, nil
		}
		if n.Name == key {
			if n.State != "removed" {
				return *n, nil
			}
			if removed == nil {
				removed = n
			}
		}
	}
	if removed != nil {
		return *removed, nil
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
			mode := "manual"
			if v.Auto {
				mode = "auto"
			}
			fmt.Fprintf(w, "policy        %d+%d (%s, version %d), chunk %s\n", v.K, v.M, mode, v.PolicyVersion, bytesStr(uint64(v.ChunkSize)))
			fmt.Fprintf(w, "inodes        %d\nchunks        %d (%d tombstoned, %d degraded, %d repair-pending, %d unreadable, %d on an older policy)\n", v.Inodes, v.Chunks, v.Tombstoned, v.Degraded, v.RepairPending, v.Unreadable, v.StalePolicy)
			fmt.Fprintf(w, "logical       %s\nstored        %s (dedup %.2fx)\n", bytesStr(v.LogicalBytes), bytesStr(v.StoredBytes), v.DedupRatio)
			fmt.Fprintf(w, "capacity      %s raw, %s usable, %s free\n", bytesStr(v.Capacity), bytesStr(v.UsableCapacity), bytesStr(v.Free))
			return nil
		},
	}
	addNodeFlags(describe)
	grow := &cobra.Command{
		Use: "grow [<disk> <size>|auto]", Short: "Raise a disk's pinned budget (edits the config and restarts the service), or explain how to add capacity",
		Long: `grow with no arguments explains the ways to add capacity. With a disk and a size it
pins that disk's budget in the config (capacity:) and restarts the service so the new
budget takes effect; "auto" removes the pin so the budget follows the disk's free space
again. The disk is named as "node ls" shows it (<host>/<n>), by its number, or by its
path. --no-restart writes the config only.`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprint(cmd.OutOrStdout(), `Add capacity:
  add a device        tailraid join <ip> <token>              (on the new device)
  add a disk          tailraid node add-disk <empty dir>      (on the device that has it)
  grow in place       tailraid volume grow <disk> <size>      (pins the budget, restarts the service)
                      tailraid volume grow <disk> auto        (the budget follows free space again)
New data spreads over the new space straight away; existing data stays where it is.
`)
				return nil
			}
			if len(args) != 2 {
				return errors.New("grow takes a disk and a size (or auto): tailraid volume grow <disk> <size>")
			}
			cfg, path, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			i, err := findDisk(cfg, args[0])
			if err != nil {
				return err
			}
			size := args[1]
			if strings.EqualFold(size, "auto") {
				size = ""
			} else if _, err := config.ParseBytes(size); err != nil {
				return fmt.Errorf("size %q: %w (examples: 500G, 1.5T, auto)", size, err)
			}
			next := cfg
			next.Node.Disks = append([]config.Disk(nil), cfg.Node.Disks...)
			next.Node.Disks[i].Capacity = size
			if err := next.Validate(); err != nil {
				return err
			}
			if err := config.Save(path, next); err != nil {
				return err
			}
			name := node.DiskName(cfg.Node.Name, i+1)
			if size == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: budget follows the disk's free space again (%s)\n", name, path)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: budget pinned at %s (%s)\n", name, size, path)
			}
			noRestart, _ := cmd.Flags().GetBool("no-restart")
			if noRestart {
				fmt.Fprintln(cmd.OutOrStdout(), "restart the service to apply it: tailraid stop && tailraid start")
				return nil
			}
			p, _, _, err := svcParams(cmd)
			if err != nil {
				return err
			}
			return restartService(cmd, &p)
		},
	}
	grow.Flags().Bool("no-restart", false, "write the config only; the budget applies at the next restart")
	c.AddCommand(describe, grow, setRedundancyCmd(), rebalanceCmd())
	return c
}

// findDisk resolves a disk of this device from the config by its name (<host>/<n>), its
// number or its path.
func findDisk(cfg config.Config, want string) (int, error) {
	if len(cfg.Node.Disks) == 0 {
		return 0, errors.New("this device has no disks (tailraid node add-disk <empty dir> adds one)")
	}
	var names []string
	for i, d := range cfg.Node.Disks {
		name := node.DiskName(cfg.Node.Name, i+1)
		names = append(names, name)
		if want == name || want == strconv.Itoa(i+1) || filepath.Clean(want) == filepath.Clean(d.Path) {
			return i, nil
		}
	}
	return 0, fmt.Errorf("no disk %q on this device; it has %s", want, strings.Join(names, ", "))
}

func resetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "reset",
		Short: "Wipe this device's local Raft state and identity so it can join afresh (shards are kept)",
		Long: `reset deletes <data_dir>/raft, node_id and cluster_id. It never touches shards/ (the
sweep reconciles them after the rejoin) or cache/. It refuses while serve is running and
while staging/ holds unflushed writes, unless --discard-dirty says to lose them.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			if yes, _ := cmd.Flags().GetBool("yes"); !yes {
				return fmt.Errorf("reset wipes %s/raft, node_id and cluster_id; re-run with --yes", cfg.Node.DataDir)
			}
			discard, _ := cmd.Flags().GetBool("discard-dirty")
			if err := cluster.Reset(cfg.Node.DataDir, discard); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "reset %s: raft state and identity removed; shards kept\n", cfg.Node.DataDir)
			return nil
		},
	}
	c.Flags().Bool("yes", false, "confirm the wipe")
	c.Flags().Bool("discard-dirty", false, "also delete staging/ (unflushed, acknowledged writes are lost)")
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
	rotate := &cobra.Command{
		Use: "rotate", Short: "Replace the join token (a Raft MetaSet); the old token stops working at once",
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := adminCall(cmd, rpc.AdminOp{Op: "rotate_token"})
			if err != nil {
				return err
			}
			var rep struct {
				JoinToken string `json:"join_token"`
			}
			if err := json.Unmarshal(raw, &rep); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), rep.JoinToken)
			return nil
		},
	}
	addNodeFlags(rotate)
	c.AddCommand(rotate)
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

// rebalanceCmd opens the leader's home gate once, or lists what it would move (docs/14 §7,
// docs/12, I-295).
func rebalanceCmd() *cobra.Command {
	var dry bool
	c := &cobra.Command{
		Use:   "rebalance",
		Short: "Move slots that are away from their home placement now, a batch each repair pass; --dry-run lists them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := adminCall(cmd, rpc.AdminOp{Op: "rebalance", DryRun: dry})
			if err != nil {
				return err
			}
			if j, _ := cmd.Flags().GetBool("json"); j {
				return printRaw(cmd.OutOrStdout(), raw)
			}
			var p repair.HomePlan
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			printHomePlan(cmd.OutOrStdout(), p, dry)
			return nil
		},
	}
	addNodeFlags(c)
	c.Flags().BoolVar(&dry, "dry-run", false, "list the moves without opening the gate")
	return c
}

func printHomePlan(w io.Writer, p repair.HomePlan, dry bool) {
	fmt.Fprintf(w, "home    %d slots away (%s), %d movable now (%s) of %s stored\n", p.AwaySlots, bytesStr(uint64(max(p.AwayBytes, 0))),
		p.MovableSlots, bytesStr(uint64(max(p.MovableBytes, 0))), bytesStr(uint64(max(p.StoredBytes, 0))))
	for _, f := range p.Flows {
		fmt.Fprintf(w, "        %s → %s  %d slots, %s\n", nodeName(f.FromName, f.From), nodeName(f.ToName, f.To), f.Slots, bytesStr(uint64(max(f.Bytes, 0))))
	}
	if stuck := p.AwaySlots - p.MovableSlots; stuck > 0 {
		fmt.Fprintf(w, "        %d away slots wait for a home that is offline or full\n", stuck)
	}
	switch {
	case dry:
		fmt.Fprintln(w, "dry run: nothing moves")
	case p.MovableSlots == 0:
		fmt.Fprintln(w, "nothing to move")
	default:
		fmt.Fprintln(w, "the home gate is open: the next repair passes move these home, a batch each pass (watch `tailraid status`)")
	}
}

func nodeName(name string, idx meta.NodeIdx) string {
	if name != "" {
		return name
	}
	return fmt.Sprintf("#%d", idx)
}

// homeLine is status's one line on home placement (docs/14 §7), empty when nothing is away.
func homeLine(h *repair.HomeStatus) string {
	if h == nil || h.AwaySlots == 0 {
		return ""
	}
	gate := fmt.Sprintf("gate closed below %.3g %%", h.Start*100)
	switch {
	case h.Forced:
		gate = "gate open (volume rebalance)"
	case h.Open:
		gate = "gate open"
	case h.Start == 0:
		gate = "gate closed (repair.home_start 0)"
	}
	return fmt.Sprintf("home    %d slots away (%s, %s movable), %s", h.AwaySlots, bytesStr(uint64(max(h.AwayBytes, 0))), bytesStr(uint64(max(h.MovableBytes, 0))), gate)
}

func setRedundancyCmd() *cobra.Command {
	var k, m int
	var auto bool
	c := &cobra.Command{
		Use:   "set-redundancy",
		Short: "Pin the volume's k+m, or --auto to let the leader choose it; existing chunks migrate in the background",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if auto {
				raw, err := adminCall(cmd, rpc.AdminOp{Op: "set_policy", Auto: true})
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "policy is automatic again: the leader applies the rule of docs/03 on its next repair pass")
				return printRaw(cmd.OutOrStdout(), raw)
			}
			kk, mm, err := policyFlags(k, m)
			if err != nil {
				return err
			}
			raw, err := adminCall(cmd, rpc.AdminOp{Op: "set_policy", K: kk, M: mm})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "policy pinned to %d+%d; existing chunks migrate on the next repair passes (watch `tailraid volume describe`)\n", k, m)
			if rep, _, _, err := fetchReport(cmd.Context(), cmd); err == nil {
				v := rep.Volume
				fmt.Fprintf(cmd.OutOrStdout(), "survives %s\n", survives(v.ToleratesDisks, v.ToleratesHosts))
				if hosts := activeHosts(rep.Nodes); v.ToleratesHosts == 0 && hosts > 1 {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: %d+%d cannot survive the loss of any one of the %d hosts (docs/14 §6)\n", k, m, hosts)
				}
			}
			return printRaw(cmd.OutOrStdout(), raw)
		},
	}
	addNodeFlags(c)
	c.Flags().IntVar(&k, "k", 0, "data shards")
	c.Flags().IntVar(&m, "m", -1, "parity shards")
	c.Flags().BoolVar(&auto, "auto", false, "let the leader choose the policy from the member table")
	return c
}

// policyFlags checks a (k, m) from the command line before the narrowing to uint8:
// k is 1..255, m is 0..255 and k+m is at most 256 (ec.MaxShards). --k 257 --m 0 used to
// pin 1+0 after the cast.
func policyFlags(k, m int) (uint8, uint8, error) {
	if k < 1 || k > 255 || m < 0 || m > 255 || k+m > 256 {
		return 0, 0, fmt.Errorf("bad policy k=%d m=%d: k is 1..255, m is 0..255, k+m at most 256", k, m)
	}
	return uint8(k), uint8(m), nil
}

// refreshUnit rewrites the systemd unit from the saved config and restarts the service.
func refreshUnit(cmd *cobra.Command) error {
	p, _, _, err := svcParams(cmd)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp("", "tailraid-install-") // a directory nobody else can pre-create (I-122)
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	p.Stage = stage
	plan, err := svc.RefreshUnit(p)
	if err != nil {
		return err
	}
	return svc.Apply(cmd.Context(), cmd.OutOrStdout(), plan)
}

// openDiskWhenUp asks the node to open a just-added disk, retrying for a minute while a
// restarted service comes back.
func openDiskWhenUp(cmd *cobra.Command, c *rpc.Client, addr string, op rpc.AdminOp) (json.RawMessage, error) {
	deadline := time.Now().Add(time.Minute)
	for {
		raw, err := c.Admin(cmd.Context(), addr, op)
		if err == nil || time.Now().After(deadline) {
			return raw, err
		}
		select {
		case <-cmd.Context().Done():
			return nil, cmd.Context().Err()
		case <-time.After(time.Second):
		}
	}
}
