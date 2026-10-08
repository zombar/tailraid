package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/update"
)

func updateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "update",
		Short: "Replace this binary with a verified release (docs/12 §5)",
		Long: `update resolves the latest GitHub release (or --version), downloads the artifact for this
OS and architecture together with the release's sha256sums.txt, verifies the checksum,
runs the download's "version", and swaps it into place. The running service is not
restarted. The repository is private: set TAILRAID_GITHUB_TOKEN or GITHUB_TOKEN, or be
logged in with the gh CLI.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			check, _ := cmd.Flags().GetBool("check")
			want, _ := cmd.Flags().GetString("version")
			yes, _ := cmd.Flags().GetBool("yes")
			out := cmd.OutOrStdout()
			u := &update.Updater{Owner: "zombar", Repo: "tailraid", Token: update.Token(ctx), Out: out}
			rel, err := u.Resolve(ctx, want)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "running %s, release %s\n", version, rel.Tag)
			newer := update.Newer(version, rel.Tag) && rel.Tag != version
			if check {
				if newer || (want != "" && rel.Tag != version) {
					fmt.Fprintln(out, "update available")
					os.Exit(2)
				}
				fmt.Fprintln(out, "up to date")
				return nil
			}
			if !newer && want == "" {
				fmt.Fprintln(out, "up to date")
				return nil
			}
			if !yes {
				fmt.Fprintf(out, "install %s over %s? [y/N] ", rel.Tag, version)
				var answer string
				_, _ = fmt.Fscanln(cmd.InOrStdin(), &answer)
				if answer != "y" && answer != "Y" {
					return errors.New("aborted")
				}
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if exe, err = filepath.EvalSymlinks(exe); err != nil {
				return err
			}
			if err := u.Apply(ctx, rel, exe, version, runtime.GOOS, runtime.GOARCH, want != ""); err != nil {
				return err
			}
			switch runtime.GOOS {
			case "windows":
				fmt.Fprintln(out, "restart the service: sc.exe stop tailraid && sc.exe start tailraid (an old copy stays beside the binary until then)")
			default:
				fmt.Fprintln(out, "restart the service: tailraid stop && tailraid start")
			}
			return nil
		},
	}
	c.Flags().Bool("check", false, "only report whether a newer release exists (exit 2 when it does)")
	c.Flags().String("version", "", "install this release tag even if it is not newer")
	c.Flags().BoolP("yes", "y", false, "do not ask before replacing the binary")
	return c
}
