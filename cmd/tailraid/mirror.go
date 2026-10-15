package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/mirror"
	"github.com/zombar/tailraid/internal/node"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/svc"
	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/vfs"
)

// mirrorCmd is `tailraid mirror` (docs/16, docs/12 §11): disks given to snapshots.
func mirrorCmd() *cobra.Command {
	c := &cobra.Command{Use: "mirror", Short: "Disks given to snapshots: add, list, remove; read one with no cluster"}
	c.AddCommand(mirrorAddCmd(), mirrorLsCmd(), mirrorRemoveCmd(), mirrorCatCmd(), mirrorExportCmd())
	return c
}

// mirrorAddCmd marks an empty directory (or a moved mirror of this cluster) as a mirror of
// this device, saves it in the config and registers it through the leader (docs/16 §1).
func mirrorAddCmd() *cobra.Command {
	var adopt bool
	c := &cobra.Command{
		Use: "add <path>", Short: "Give a disk of this device to snapshots (a moved mirror keeps its history; --adopt takes over another cluster's)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, path, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			p, err := filepath.Abs(filepath.Clean(args[0]))
			if err != nil {
				return err
			}
			next := cfg
			listed := false
			for _, m := range cfg.Node.Mirrors {
				listed = listed || filepath.Clean(m.Path) == p
			}
			if !listed {
				next.Node.Mirrors = append(append([]config.Mirror(nil), cfg.Node.Mirrors...), config.Mirror{Path: p})
			}
			if err := next.Validate(); err != nil {
				return err
			}
			rep, c, addr, err := fetchReport(cmd.Context(), cmd)
			if err != nil {
				return err
			}
			host, n := selfDevice(rep)
			if host == nil {
				return errors.New("this device is not in the node table")
			}
			cid, err := cluster.LoadClusterID(cfg.Node.DataDir)
			if err != nil {
				return err
			}
			id, err := node.PrepareMirror(p, cid, host.ID, adopt)
			if err != nil {
				return err
			}
			if !listed {
				if err := config.Save(path, next); err != nil {
					return err
				}
			}
			name := fmt.Sprintf("%s/m%d", host.Name, n)
			if err := c.Members(cmd.Context(), leaderOr(rep, addr), rpc.MemberOp{Op: "add_mirror", NodeIdx: host.Idx, DiskID: id, Name: name}); err != nil {
				return fmt.Errorf("register mirror at %s (the marker and the config entry stay; re-run to retry): %w", p, err)
			}
			// a systemd service may write only what its unit names: rewrite the unit with the
			// mirror and restart, as add-disk does (I-147)
			if runtime.GOOS == "linux" && svc.UnitInstalled() {
				if err := refreshUnit(cmd); err != nil {
					return fmt.Errorf("registered mirror %s, but the service unit was not updated (run `tailraid install`, then restart the service): %w", name, err)
				}
			}
			raw, err := openDiskWhenUp(cmd, c, addr, rpc.AdminOp{Op: "open_mirror", Path: p})
			if err != nil {
				return fmt.Errorf("registered mirror %s; the node did not open it yet (it looks again within a minute): %w", name, err)
			}
			var st struct {
				State string `json:"state"`
			}
			_ = json.Unmarshal(raw, &st)
			if rep, _, _, err := fetchReport(cmd.Context(), cmd); err == nil { // a re-home keeps the row's name
				for i := range rep.Nodes {
					if rep.Nodes[i].ID == id && rep.Nodes[i].State != meta.Removed.String() {
						name = rep.Nodes[i].Name
					}
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "mirror %s at %s: %s (`tailraid snapshot ls` lists its snapshots; the schedule takes one when due)\n", name, p, st.State)
			return nil
		},
	}
	c.Flags().BoolVar(&adopt, "adopt", false, "take over a mirror of another cluster: rewrite its marker and import its snapshots (docs/16 §7)")
	addNodeFlags(c)
	return c
}

// selfDevice is this device's row in a report and the number its next mirror takes.
func selfDevice(rep admin.Report) (*admin.NodeReport, int) {
	var host *admin.NodeReport
	for i := range rep.Nodes {
		if rep.Nodes[i].Self && rep.Nodes[i].Kind == meta.KindDevice.String() {
			host = &rep.Nodes[i]
		}
	}
	n := 1
	for i := range rep.Nodes {
		if host != nil && rep.Nodes[i].Kind == meta.KindMirror.String() && rep.Nodes[i].Host == host.Idx {
			n++
		}
	}
	return host, n
}

// mirrorLsCmd lists the cluster's mirrors, or with a path reads that disk's snapshots
// with no cluster (docs/16 §7).
func mirrorLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use: "ls [path]", Short: "List the mirrors and this device's view of them; with a path, the snapshots on that disk (no cluster needed)", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return standaloneLs(cmd, args[0])
			}
			rep, _, _, err := fetchReport(cmd.Context(), cmd)
			if err != nil {
				return err
			}
			if j, _ := cmd.Flags().GetBool("json"); j {
				return printJSON(cmd.OutOrStdout(), rep.Mirrors)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tHOST\tSTATE\tUSED\tFREE\tNEWEST\tPATH")
			for i := range rep.Nodes {
				nd := &rep.Nodes[i]
				if nd.Kind != meta.KindMirror.String() || nd.State == meta.Removed.String() {
					continue
				}
				state, used, free, newest, path := "elsewhere", "-", "-", "-", "-"
				for j := range rep.Mirrors {
					m := &rep.Mirrors[j]
					if m.Name == nd.Name {
						state, used, free, path = "offline", bytesStr(uint64(max(m.Used, 0))), bytesStr(m.Free), m.Path
						if m.Online {
							state = "online"
						}
						if m.Syncing != nil {
							state += fmt.Sprintf(", syncing %s %d %%", m.Syncing.Snapshot, m.Syncing.Percent())
						}
						if m.Newest != 0 {
							newest = time.Unix(0, m.Newest).Local().Format("2006-01-02 15:04")
						}
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", nd.Name, hostName(rep, nd.Host), state, used, free, newest, path)
			}
			return w.Flush()
		},
	}
	addNodeFlags(c)
	return c
}

func hostName(rep admin.Report, idx meta.NodeIdx) string {
	for i := range rep.Nodes {
		if rep.Nodes[i].Idx == idx {
			return rep.Nodes[i].Name
		}
	}
	return "-"
}

func standaloneLs(cmd *cobra.Command, path string) error {
	m, err := mirror.Open(sysfs.OS{}, path, "") // read only: nothing on the disk changes
	if err != nil {
		return err
	}
	mans, err := m.Manifests()
	if err != nil {
		return err
	}
	if j, _ := cmd.Flags().GetBool("json"); j {
		return printJSON(cmd.OutOrStdout(), mans)
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "mirror %s of cluster %s, %s on the disk\n", m.Marker().MirrorID, m.Marker().ClusterID, bytesStr(uint64(max(m.Used(), 0))))
	fmt.Fprintln(w, "NAME\tTAKEN\tFILES\tBYTES\tADDED\tMISSING")
	for i := range mans {
		man := &mans[i]
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%d\n", man.Name, time.Unix(0, man.At).Local().Format("2006-01-02 15:04"), man.Files, bytesStr(man.Bytes), bytesStr(man.NewBytes), len(man.Missing))
	}
	return w.Flush()
}

// openSnapshot opens the mirror at path read-only and finds the snapshot by name.
func openSnapshot(path, name string) (*mirror.Mirror, mirror.Manifest, error) {
	m, err := mirror.Open(sysfs.OS{}, path, "")
	if err != nil {
		return nil, mirror.Manifest{}, err
	}
	mans, err := m.Manifests()
	if err != nil {
		return nil, mirror.Manifest{}, err
	}
	for i := range mans {
		if mans[i].Name == name {
			return m, mans[i], nil
		}
	}
	return nil, mirror.Manifest{}, fmt.Errorf("%w: no snapshot %q on %s", mirror.ErrNotFound, name, path)
}

func mirrorCatCmd() *cobra.Command {
	return &cobra.Command{
		Use: "cat <mirror path> <snapshot> <file>", Short: "Print a file of a snapshot from a mirror disk (no cluster needed)", Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, man, err := openSnapshot(args[0], args[1])
			if err != nil {
				return err
			}
			e, err := m.Resolve(meta.Hash(man.Root), args[2])
			if err != nil {
				return err
			}
			return m.ReadFile(e, man.ChunkSize, cmd.OutOrStdout())
		},
	}
}

func mirrorExportCmd() *cobra.Command {
	var sub string
	c := &cobra.Command{
		Use: "export <mirror path> <snapshot> <destination dir>", Short: "Copy a snapshot (or --path, a part of it) out of a mirror disk into a local directory (no cluster needed)", Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, man, err := openSnapshot(args[0], args[1])
			if err != nil {
				return err
			}
			if err := os.MkdirAll(args[2], 0o700); err != nil {
				return err
			}
			if err := m.Export(man, sub, args[2]); err != nil {
				return fmt.Errorf("export finished with errors (every other file was written): %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "snapshot %s exported to %s\n", man.Name, args[2])
			return nil
		},
	}
	c.Flags().StringVar(&sub, "path", "/", "the folder or file of the snapshot to export")
	return c
}

func mirrorRemoveCmd() *cobra.Command {
	c := &cobra.Command{
		Use: "remove <name>", Short: "Forget a mirror; the disk and every snapshot on it stay as they are (added back, they return)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, c, addr, err := fetchReport(cmd.Context(), cmd)
			if err != nil {
				return err
			}
			for i := range rep.Nodes {
				nd := &rep.Nodes[i]
				if nd.Kind == meta.KindMirror.String() && nd.Name == args[0] && nd.State != meta.Removed.String() {
					return c.Members(cmd.Context(), leaderOr(rep, addr), rpc.MemberOp{Op: "remove", NodeIdx: nd.Idx})
				}
			}
			return fmt.Errorf("%w: no mirror %q", meta.ErrNoSuchNode, args[0])
		},
	}
	addNodeFlags(c)
	return c
}

// snapshotCmd is `tailraid snapshot` (docs/16, docs/12 §11). There is no removal verb:
// only the schedule thins automatic snapshots, and a manual one is permanent (I-183).
func snapshotCmd() *cobra.Command {
	c := &cobra.Command{Use: "snapshot", Short: "Snapshots of the volume on the mirrors: list and take (none is ever removed by hand)"}
	var mirrorName string
	ls := &cobra.Command{
		Use: "ls", Short: "List every snapshot on every mirror, online or not", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := adminCall(cmd, rpc.AdminOp{Op: "snapshot_ls", Mirror: mirrorName})
			if err != nil {
				return err
			}
			if j, _ := cmd.Flags().GetBool("json"); j {
				return printRaw(cmd.OutOrStdout(), raw)
			}
			var rows []node.SnapshotInfo
			if err := json.Unmarshal(raw, &rows); err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tMIRROR\tTAKEN\tKIND\tSTATE\tFILES\tBYTES\tADDED")
			for i := range rows {
				r := &rows[i]
				kind := "manual"
				if r.Auto {
					kind = "auto"
				}
				state := r.State
				if r.Missing > 0 {
					state = fmt.Sprintf("%s (%d missing)", state, r.Missing)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", r.Name, r.Mirror, time.Unix(0, r.At).Local().Format("2006-01-02 15:04"), kind, state, r.Files, bytesStr(r.Bytes), bytesStr(r.NewBytes))
			}
			return w.Flush()
		},
	}
	ls.Flags().StringVar(&mirrorName, "mirror", "", "one mirror by name")
	addNodeFlags(ls)
	create := &cobra.Command{
		Use: "create [name]", Short: "Take a manual snapshot now on every online mirror (or --mirror); a manual snapshot is permanent", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			op := rpc.AdminOp{Op: "snapshot_create", Mirror: mirrorName}
			if len(args) == 1 {
				op.Name = args[0]
			}
			raw, err := adminCall(cmd, op)
			if err != nil {
				return err
			}
			var rows []node.SnapshotInfo
			if err := json.Unmarshal(raw, &rows); err != nil {
				return err
			}
			for _, r := range rows {
				fmt.Fprintf(cmd.OutOrStdout(), "snapshot %s begun on %s (`tailraid snapshot ls` shows when it completes)\n", r.Name, r.Mirror)
			}
			return nil
		},
	}
	create.Flags().StringVar(&mirrorName, "mirror", "", "one mirror by name")
	addNodeFlags(create)
	var to, as string
	restore := &cobra.Command{
		Use:   "restore <mirror>/<name> <path>",
		Short: "Copy a file or folder of a snapshot into the live volume (an existing file is skipped, never overwritten)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, name, err := splitSnapshotRef(args[0])
			if err != nil {
				return err
			}
			raw, err := localCall(cmd, rpc.AdminOp{Op: "snapshot_restore", Mirror: m, Name: name, Path: args[1], To: to, As: as})
			if err != nil {
				return err
			}
			return printRestore(cmd.OutOrStdout(), raw)
		},
	}
	restore.Flags().StringVar(&to, "to", "", "the folder to restore into (default: where it was)")
	restore.Flags().StringVar(&as, "as", "", "the name to restore it as")
	addNodeFlags(restore)
	c.AddCommand(ls, create, restore)
	return c
}

// splitSnapshotRef splits <mirror>/<name>: the mirror's name holds a slash (pike/m1), so
// the snapshot's name is what follows the last one.
func splitSnapshotRef(ref string) (mirrorName, name string, err error) {
	i := strings.LastIndexByte(ref, '/')
	if i <= 0 || i == len(ref)-1 {
		return "", "", fmt.Errorf("%q: want <mirror>/<name>, e.g. pike/m1/2026-10-12T1400", ref)
	}
	return ref[:i], ref[i+1:], nil
}

func printRestore(w io.Writer, raw json.RawMessage) error {
	var r struct {
		Path   string            `json:"path"`
		Report vfs.RestoreReport `json:"report"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	fmt.Fprintf(w, "restored to %s: %d files, %d folders, %d links, %d chunks read from the mirror\n", r.Path, r.Report.Files, r.Report.Dirs, r.Report.Links, r.Report.Uploaded)
	for _, s := range r.Report.Skipped {
		fmt.Fprintf(w, "  skipped %s (exists)\n", s)
	}
	return nil
}

func versionsCmd() *cobra.Command {
	var revert, copyFrom, as string
	c := &cobra.Command{
		Use:   "versions <path>",
		Short: "The versions of a file on the online mirrors; --revert or --copy one back",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			op := rpc.AdminOp{Op: "versions", Path: args[0]}
			switch {
			case revert != "" && copyFrom != "":
				return errors.New("--revert or --copy, not both")
			case revert != "":
				op.Op = "versions_revert"
				var err error
				if op.Mirror, op.Name, err = splitSnapshotRef(revert); err != nil {
					return err
				}
			case copyFrom != "":
				if as == "" {
					return errors.New("--copy needs --as <name>")
				}
				op.Op, op.As = "versions_copy", as
				var err error
				if op.Mirror, op.Name, err = splitSnapshotRef(copyFrom); err != nil {
					return err
				}
			}
			raw, err := localCall(cmd, op)
			if err != nil {
				return err
			}
			switch op.Op {
			case "versions_revert":
				fmt.Fprintf(cmd.OutOrStdout(), "%s now holds its content of %s\n", args[0], revert)
				return nil
			case "versions_copy":
				return printRestore(cmd.OutOrStdout(), raw)
			}
			var r node.VersionsReply
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "VERSION\tSIZE\tMODIFIED\tIN SNAPSHOTS")
			for i := range r.Versions {
				v := &r.Versions[i]
				label := fmt.Sprint(i)
				if v.Current {
					label = "current"
				}
				var in []string
				for _, s := range v.Snapshots {
					in = append(in, s.Mirror+"/"+s.Name)
				}
				held := strings.Join(in, ", ")
				if held == "" {
					held = "in no snapshot yet"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", label, bytesStr(v.Size), time.Unix(0, v.Mtime).Local().Format("2006-01-02 15:04"), held)
			}
			for _, o := range r.Offline {
				fmt.Fprintf(w, "\t\t\t%d snapshots on %s, offline\n", o.Snapshots, o.Mirror)
			}
			return w.Flush()
		},
	}
	c.Flags().StringVar(&revert, "revert", "", "make the file hold its content of <mirror>/<name>")
	c.Flags().StringVar(&copyFrom, "copy", "", "copy the file's content of <mirror>/<name> beside it")
	c.Flags().StringVar(&as, "as", "", "the copy's name (with --copy)")
	addNodeFlags(c)
	return c
}
