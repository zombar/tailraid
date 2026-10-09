package main

import (
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/soak"
)

func verifyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "verify",
		Short: "Run the soak scenarios against the live cluster through this device's mount and write an HTML report",
		Long: `verify writes, reads, churns, deduplicates and garbage-collects under /_soak/<timestamp>/ on
this device's mount, compares every node's FSM state, and records the result in an HTML
report (docs/09 §6). Phases that need node kills or partitions are reported as skipped;
--destructive adds shard corruption on nodes that run with dev.enabled and allows the
phases that change the cluster itself (policy changes, drains, a disk fill); without it
the volume's files are the only thing touched. The GC phase waits the cluster's
repair.grace. Ctrl-C stops after the current phase, restores what it changed and writes
the partial report.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cl, addr, cfg, err := localClient(ctx, cmd)
			if err != nil {
				return err
			}
			cid, err := cluster.LoadClusterID(cfg.Node.DataDir)
			if err != nil {
				return err
			}
			if _, err := os.Stat(cfg.Mount.Path); err != nil {
				return fmt.Errorf("mount path %s: %w (run `tailraid mount` first)", cfg.Mount.Path, err)
			}
			grace, fillAfter, err := cfg.RepairDurations()
			if err != nil {
				return err
			}
			destructive, _ := cmd.Flags().GetBool("destructive")
			restarts, _ := cmd.Flags().GetBool("restarts")
			if restarts && !destructive {
				return fmt.Errorf("--restarts needs --destructive")
			}
			out, _ := cmd.Flags().GetString("report")
			resultsPath, _ := cmd.Flags().GetString("results")
			seed, _ := cmd.Flags().GetInt64("seed")
			if seed == 0 {
				seed = time.Now().UnixNano()
			}
			p := soak.Quick()
			p.Name, p.Grace, p.FillAfter = "live", grace, fillAfter
			p.ChunkSize = 0 // taken from the cluster's status unless --chunk overrides it
			if cs, _ := cmd.Flags().GetInt("chunk"); cs > 0 {
				p.ChunkSize = cs
			}
			env := soak.NewLiveEnv(cl, addr, cfg.Mount.Path, fmt.Sprintf("%x", cid[:4]), destructive, restarts)
			rf, err := os.Create(resultsPath) //nolint:gosec // --results names the file
			if err != nil {
				return err
			}
			defer rf.Close()
			log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
			var flags []string
			if destructive {
				flags = append(flags, "destructive")
			}
			if restarts {
				flags = append(flags, "restarts")
			}
			run := &soak.Run{Env: env, Profile: p, Log: log, Out: rf, Rng: rand.New(rand.NewSource(seed)), Seed: seed, Flags: flags} //nolint:gosec // workload
			log.Info("verify", "cluster", env.ClusterID, "mount", cfg.Mount.Path, "seed", seed, "destructive", destructive, "restarts", restarts)
			// Ctrl-C ends the run after the current phase: the phase restores what it
			// changed, the run directory is removed and the partial report is written
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			sum, runErr := run.Execute(ctx)
			if sum.Aborted != "" {
				log.Warn("verify stopped early", "reason", sum.Aborted)
			}
			f, err := os.Create(out) //nolint:gosec // --report names the file
			if err != nil {
				return errors.Join(runErr, err)
			}
			if err := soak.Render(f, sum); err != nil {
				_ = f.Close()
				return errors.Join(runErr, err)
			}
			_ = f.Close()
			w := cmd.OutOrStdout()
			for _, ph := range sum.Phases {
				state := "PASS"
				switch {
				case ph.NotRun:
					state = "NOT RUN"
				case ph.Skipped:
					state = "SKIPPED (" + ph.Reason + ")"
				case ph.Failed > 0:
					state = fmt.Sprintf("FAIL (%d)", ph.Failed)
				}
				fmt.Fprintf(w, "%-28s %s  %s\n", ph.Name, state, ph.Took.Round(time.Millisecond))
			}
			abs, _ := filepath.Abs(out)
			fmt.Fprintf(w, "%d failed checks; report: %s\n", sum.Failed, abs)
			if runErr != nil {
				return fmt.Errorf("verify aborted: %w (partial report: %s)", runErr, abs)
			}
			if sum.Failed > 0 {
				return fmt.Errorf("verify found %d failed checks", sum.Failed)
			}
			return nil
		},
	}
	addNodeFlags(c)
	c.Flags().String("report", "report.html", "HTML report path")
	c.Flags().String("results", "results.jsonl", "results stream path")
	c.Flags().Bool("destructive", false, "also corrupt shards (targets need dev.enabled)")
	c.Flags().Bool("restarts", false, "with --destructive: every target runs under a restarting service manager, so kill, restart and partition phases run through /dev/exit and /dev/fault")
	c.Flags().Int64("seed", 0, "workload seed (0 = time)")
	c.Flags().Int("chunk", 0, "the volume's chunk size in bytes (default: the quick profile's 256 KiB)")
	return c
}
