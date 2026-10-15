// Command tailraid is the node daemon, gateway and management CLI (docs/00).
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/svc"
)

var version = "dev"

func main() {
	root := &cobra.Command{
		Use:           "tailraid",
		Short:         "One erasure-coded, deduplicated volume across your tailnet",
		SilenceUsage:  true,
		SilenceErrors: true,
		// every command logs in the configured format (log.level/log.format, env overrides)
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			slog.SetDefault(cfg.Logger())
			return nil
		},
	}
	addRootFlags(root)
	root.AddCommand(initCmd(), joinCmd(), serveCmd(), resetCmd(), statusCmd(), dashboardCmd(), nodeCmd(), volumeCmd(), tokenCmd(), checkCmd(), repairCmd(), gcCmd(), trashCmd(), cacheCmd(), fsckCmd(), chunkCmd(), verifyCmd(), mountCmd(), umountCmd(), installCmd(), uninstallCmd(), startStopCmd("start", svc.Start), startStopCmd("stop", svc.Stop), logsCmd(), updateCmd(), exportCmd(), recoverCmd(), mirrorCmd(), snapshotCmd(), versionsCmd(), statsCmd(), searchCmd(), mediaCmd(), secretCmd(), userCmd(), groupCmd(), grantCmd(), revokeCmd(), accessCmd(), webCmd(), auditCmd(), versionCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "tailraid:", err)
		os.Exit(1)
	}
}

// addRootFlags adds the flags every command takes.
func addRootFlags(root *cobra.Command) {
	root.PersistentFlags().String("config", "", "config file (default: per-OS tailraid/config.yaml)")
	root.PersistentFlags().String("identity", "", "sign this one command as another identity this OS account holds (docs/21 §1)")
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "tailraid %s\n", version)
		},
	}
}
