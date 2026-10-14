package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/raft"
	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/rpc"
)

// exportCmd copies the newest metadata snapshot out of the running node (docs/12 §9); it
// was `tailraid snapshot <dir>` until the volume snapshots of docs/16 took that word.
func exportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export <dir>",
		Short: "Export the cluster metadata (a fresh Raft snapshot) into a directory",
		Long: `export asks the local node for a fresh Raft snapshot and copies it to <dir>/<id>/
(state.bin and meta.json). It is the metadata only: file names, chunk hashes and where
each shard lives. Shards stay on their nodes. Keep a recent copy somewhere outside the
cluster; "tailraid recover" rebuilds a node's membership from the node's own newest
snapshot and log after a majority is lost (docs/12 §9).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, addr, _, err := localClient(cmd)
			if err != nil {
				return err
			}
			raw, err := c.Admin(ctx, addr, rpc.AdminOp{Op: "export"})
			if err != nil {
				return err
			}
			var res struct {
				Dir   string `json:"dir"`
				Index uint64 `json:"index"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("export reply: %w", err)
			}
			dst := filepath.Join(args[0], filepath.Base(res.Dir))
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return err
			}
			if err := os.CopyFS(dst, os.DirFS(res.Dir)); err != nil {
				return fmt.Errorf("copy %s to %s: %w", res.Dir, dst, err)
			}
			_ = os.RemoveAll(res.Dir)
			fmt.Fprintf(cmd.OutOrStdout(), "metadata snapshot at index %d written to %s\n", res.Index, dst)
			return nil
		},
	}
}

// recoverCmd rewrites a stopped node's Raft membership (docs/12 §9).
func recoverCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "recover --voters <node-id>[,<node-id>...]",
		Short: "After a lost majority: make this stopped node continue with the given voters",
		Long: `recover is the last resort of docs/05 (h): when more voters are gone for good than Raft can
lose, the survivors can never elect a leader. On one survivor, with the service stopped,
recover rewrites the Raft membership to the voters named (this node must be one of them,
by node ID as "node ls" shows it) using the node's own newest snapshot and log, so it
starts as the leader of that set with every file whose shards survive still readable.
Run it on one node only; the others rejoin as fresh members (reset, then join).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			list, _ := cmd.Flags().GetString("voters")
			var ids []string
			for id := range strings.SplitSeq(list, ",") {
				if id = strings.TrimSpace(id); id != "" {
					ids = append(ids, id)
				}
			}
			if len(ids) == 0 {
				return errors.New("--voters names at least this node")
			}
			if !cluster.HasState(cfg.Node.DataDir) {
				return fmt.Errorf("%s holds no Raft state (nothing to recover)", cfg.Node.DataDir)
			}
			id, err := cluster.LoadOrCreateNodeID(cfg.Node.DataDir)
			if err != nil {
				return err
			}
			cid, err := cluster.LoadClusterID(cfg.Node.DataDir)
			if err != nil {
				return err
			}
			o := cluster.Options{DataDir: cfg.Node.DataDir, NodeID: id, ClusterID: cid, Logger: cfg.Logger()}
			members, err := cluster.OfflineMembers(o)
			if err != nil {
				if errors.Is(err, cluster.ErrAlreadyOpen) {
					return errors.New("the service is running: stop it first (tailraid stop)")
				}
				return err
			}
			var servers []raft.Server
			for _, want := range ids {
				found := false
				for i := range members {
					if m := &members[i]; m.ID == want {
						servers = append(servers, raft.Server{Suffrage: raft.Voter, ID: raft.ServerID(m.ID), Address: raft.ServerAddress(m.RaftAddr())})
						found = true
					}
				}
				if !found {
					known := make([]string, 0, len(members))
					for i := range members {
						known = append(known, members[i].ID+" ("+members[i].Name+")")
					}
					return fmt.Errorf("node %s is not in the member table; known: %s", want, strings.Join(known, ", "))
				}
			}
			if err := cluster.Recover(o, servers); err != nil {
				if errors.Is(err, cluster.ErrAlreadyOpen) {
					return errors.New("the service is running: stop it first (tailraid stop)")
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "recovered: this node continues with voters %s; start the service (tailraid start), then reset and re-join the other devices\n", strings.Join(ids, ", "))
			return nil
		},
	}
	c.Flags().String("voters", "", "comma-separated node IDs that remain voters (this node included)")
	_ = c.MarkFlagRequired("voters")
	return c
}
