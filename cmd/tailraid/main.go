// Command tailraid is the node daemon, gateway and management CLI (docs/00).
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	root := &cobra.Command{
		Use:           "tailraid",
		Short:         "One erasure-coded, deduplicated volume across your tailnet",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("config", "", "config file (default: per-OS tailraid/config.yaml)")
	root.AddCommand(initCmd(), serveCmd(), mountCmd(), umountCmd(), versionCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "tailraid:", err)
		os.Exit(1)
	}
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
