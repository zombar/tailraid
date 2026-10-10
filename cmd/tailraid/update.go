package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/svc"
	"github.com/zombar/tailraid/internal/update"
)

func updateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "update",
		Short: "Replace this binary with a verified release (docs/12 §5)",
		Long: `update resolves the latest GitHub release (or --version), downloads the artifact for this
OS and architecture together with the release's sha256sums.txt, verifies the checksum,
runs the download's "version", and swaps it into place; the release's sha256sums.txt
must carry the minisign signature of the release key (docs/12 §4). The previous binary
stays as <exe>.old until the next clean start; --rollback puts it back. The running
service is not restarted unless --restart is given. The repository is public, so no
token is needed; one is read from TAILRAID_GITHUB_TOKEN, GITHUB_TOKEN or the gh CLI when
present (a private fork needs it).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			check, _ := cmd.Flags().GetBool("check")
			want, _ := cmd.Flags().GetString("version")
			yes, _ := cmd.Flags().GetBool("yes")
			allowDowngrade, _ := cmd.Flags().GetBool("allow-downgrade")
			rollback, _ := cmd.Flags().GetBool("rollback")
			restart, _ := cmd.Flags().GetBool("restart")
			out := cmd.OutOrStdout()
			// the service parameters are resolved before any swap: on Linux the running
			// binary's path (/proc/self/exe) follows the file, which the swap renames away
			var params *svc.Params
			if restart {
				p, _, _, err := svcParams(cmd)
				if err != nil {
					return err
				}
				params = &p
			}
			if rollback {
				exe, err := executable()
				if err != nil {
					return err
				}
				if err := update.Rollback(exe); err != nil {
					return err
				}
				fmt.Fprintf(out, "restored the previous binary at %s\n", exe)
				return restartService(cmd, params)
			}
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
			exe, err := executable()
			if err != nil {
				return err
			}
			if err := u.Apply(ctx, rel, exe, version, runtime.GOOS, runtime.GOARCH, want != "", allowDowngrade); err != nil {
				return err
			}
			if !restart {
				switch runtime.GOOS {
				case "windows":
					fmt.Fprintln(out, "restart the service: sc.exe stop tailraid && sc.exe start tailraid (the previous binary stays beside the new one as .old)")
				default:
					fmt.Fprintln(out, "restart the service: tailraid stop && tailraid start (or update --restart next time)")
				}
			}
			return restartService(cmd, params)
		},
	}
	c.Flags().Bool("check", false, "only report whether a newer release exists (exit 2 when it does)")
	c.Flags().String("version", "", "install this release tag even if it is not newer")
	c.Flags().BoolP("yes", "y", false, "do not ask before replacing the binary")
	c.Flags().Bool("allow-downgrade", false, "with --version: install a release older than the running one")
	c.Flags().Bool("rollback", false, "put the previous binary (<exe>.old) back instead of updating")
	c.Flags().Bool("restart", false, "stop and start the installed service after the swap")
	return c
}

// executable is the resolved path of this binary.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// restartService stops and starts the installed service when asked (p nil: not asked),
// with parameters resolved before the binary was swapped. The stop plan returns only
// once the old process has exited, so the start runs the new binary (I-131).
func restartService(cmd *cobra.Command, p *svc.Params) error {
	if p == nil {
		return nil
	}
	stop, err := svc.Stop(*p)
	if err != nil {
		return err
	}
	start, err := svc.Start(*p)
	if err != nil {
		return err
	}
	if err := svc.Apply(cmd.Context(), cmd.OutOrStdout(), stop); err != nil {
		return err
	}
	return svc.Apply(cmd.Context(), cmd.OutOrStdout(), start)
}
