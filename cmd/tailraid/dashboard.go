package main

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/admin"
)

// dashboardCmd opens the node's browser UI (docs/13 §1, I-143).
func dashboardCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "dashboard",
		Short: "Open the browser UI (dashboard and file browser) in the default browser",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep, _, _, err := fetchReport(cmd.Context(), cmd)
			if err != nil {
				return err
			}
			u, err := dashboardURL(rep)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), u)
			if print, _ := cmd.Flags().GetBool("print"); print {
				return nil
			}
			argv, err := browserCommand(runtime.GOOS, u)
			if err != nil {
				return err
			}
			if err := exec.CommandContext(cmd.Context(), argv[0], argv[1:]...).Run(); err != nil { //nolint:gosec // the OS opener with the node's own URL as one argument (I-143)
				return fmt.Errorf("opening the browser (%s): %w; open the address above yourself", argv[0], err)
			}
			return nil
		},
	}
	c.Flags().String("node", "", "RPC address of the node to talk to (default: this device)")
	c.Flags().Bool("print", false, "print the address without opening a browser")
	return c
}

// dashboardURL is the browser UI address the node reports, or why there is none.
func dashboardURL(rep admin.Report) (string, error) {
	if rep.WebURL == "" {
		return "", errors.New("the node serves no browser UI: listen.web is 0 in its config (docs/13)")
	}
	return rep.WebURL, nil
}

// browserCommand is the OS's own "open this URL" program with the URL as one argument:
// no shell, so nothing in the URL is interpreted.
func browserCommand(goos, url string) ([]string, error) {
	switch goos {
	case "darwin":
		return []string{"open", url}, nil
	case "linux":
		return []string{"xdg-open", url}, nil
	case "windows":
		return []string{"rundll32", "url.dll,FileProtocolHandler", url}, nil
	}
	return nil, fmt.Errorf("no browser opener known on %s; open %s yourself", goos, url)
}
