package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/scrape"
	"github.com/zombar/tailraid/internal/search"
	"github.com/zombar/tailraid/internal/webui"
)

func mediaCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "media",
		Short: "Media library tools: fill missing sidecars and art from TMDB and MusicBrainz (docs/18 §11)",
	}
	c.AddCommand(mediaKeyCmd(), mediaScrapeCmd())
	return c
}

func mediaKeyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "key",
		Short: "Save a TMDB API key on this device (from " + scrape.KeyEnv + " or stdin)",
		Long: `key reads a TMDB API key (a v3 key or a v4 read token, free with a TMDB account) from
` + scrape.KeyEnv + ` or stdin, checks it with TMDB and keeps it in <data_dir>/` + scrape.KeyFile + `,
mode 0600, on this device only.

` + scrape.Attribution,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			key := strings.TrimSpace(os.Getenv(scrape.KeyEnv))
			if key == "" {
				fmt.Fprint(cmd.ErrOrStderr(), "TMDB API key: ")
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && line == "" {
					return fmt.Errorf("no key (set %s or type it on stdin): %w", scrape.KeyEnv, err)
				}
				key = strings.TrimSpace(line)
			}
			if key == "" {
				return fmt.Errorf("empty key (set %s or type it on stdin)", scrape.KeyEnv)
			}
			if err := scrape.New(key, version).CheckKey(cmd.Context()); err != nil {
				return fmt.Errorf("check the key with TMDB (nothing was saved): %w", err)
			}
			if err := scrape.SaveKey(cfg.Node.DataDir, key); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "saved the TMDB key on this device\n%s\n", scrape.Attribution)
			return nil
		},
	}
	return c
}

func mediaScrapeCmd() *cobra.Command {
	var o scrape.Options
	var only []string
	c := &cobra.Command{
		Use:   "scrape [path]...",
		Short: "Write the missing .nfo sidecars, posters, backdrops, stills and covers of the libraries",
		Long: `scrape lists the library items under the paths (every library when none is given) that
lack a sidecar or art, looks each one up with TMDB (films, shows, seasons, episodes) or
MusicBrainz and the Cover Art Archive (album covers) or the libretro thumbnails (box art
of a game, by its No-Intro name), and writes only what is missing,
under the names the Media tab reads. Nothing that exists is replaced, renamed or
deleted. A clear match (title and year agree) is used at once; any other is listed for
you to choose on a terminal, skipped without one, or taken with --yes. Only this
command contacts a provider: no node ever does (docs/18 §11).

` + scrape.Attribution,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, _, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			key, err := scrape.LoadKey(cfg.Node.DataDir)
			if err != nil {
				return err
			}
			o.Only = map[string]bool{}
			for _, g := range only {
				switch g {
				case scrape.OnlyMovies, scrape.OnlyTV, scrape.OnlyEpisodes, scrape.OnlyMusic, scrape.OnlyGames:
					o.Only[g] = true
				default:
					return fmt.Errorf("--only %q: want movies, tv, episodes, music or games", g)
				}
			}
			if len(args) == 0 {
				args = []string{"/"}
			}
			if o.ID != "" && len(args) != 1 {
				return errors.New("--id names one item: give one path")
			}
			rep, _, _, err := fetchReport(ctx, cmd)
			if err != nil {
				return err
			}
			base, err := dashboardURL(rep)
			if err != nil {
				return fmt.Errorf("scrape writes through the node's web listener: %w", err)
			}
			items, err := scrapePlan(cmd, args)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			o.Out = out
			if len(items) == 0 {
				fmt.Fprintln(out, "nothing is missing")
				return nil
			}
			var ch scrape.Chooser
			if isTerminal(os.Stdin) {
				ch = &ttyChooser{in: bufio.NewReader(cmd.InOrStdin()), out: cmd.ErrOrStderr()}
			}
			w := &webWriter{base: strings.TrimSuffix(base, "/"), http: &http.Client{Timeout: 2 * time.Minute}}
			sum, err := scrape.Run(ctx, scrape.New(key, version), items, w, ch, o)
			fmt.Fprintf(out, "%d files for %d items", sum.Files, sum.Items)
			if sum.Present > 0 {
				fmt.Fprintf(out, " · %d appeared meanwhile and were left alone", sum.Present)
			}
			fmt.Fprintln(out)
			for _, s := range sum.Skipped {
				fmt.Fprintf(out, "skipped %s: %s\n", s.Path, s.Why)
			}
			if err != nil {
				return err
			}
			if sum.Files > 0 && !o.DryRun {
				if _, err := adminCall(cmd, rpc.AdminOp{Op: "index_now"}); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "the index pass did not run now (%v): the next one picks the files up\n", err)
				}
			}
			fmt.Fprintln(out, scrape.Attribution)
			return nil
		},
	}
	c.Flags().StringSliceVar(&only, "only", nil, "only these groups: movies, tv, episodes, music, games (default all)")
	c.Flags().StringVar(&o.Lang, "lang", "en-US", "the TMDB language")
	c.Flags().StringVar(&o.Region, "region", "US", "the certification country")
	c.Flags().BoolVar(&o.Yes, "yes", false, "take the top result when a search has no clear match")
	c.Flags().BoolVar(&o.DryRun, "dry-run", false, "print what would be written, write nothing")
	c.Flags().StringVar(&o.ID, "id", "", "the provider id of the one item the path names: tmdb:<n>, mbid:<uuid> or libretro:<name>")
	addNodeFlags(c)
	return c
}

// scrapePlan asks this node for the plan under each path, a page at a time, each item once.
func scrapePlan(cmd *cobra.Command, paths []string) ([]search.ScrapeItem, error) {
	var out []search.ScrapeItem
	seen := map[uint64]bool{}
	for _, p := range paths {
		cursor := ""
		for {
			raw, err := localCall(cmd, rpc.AdminOp{Op: "scrape_plan", Path: p, Cursor: cursor})
			if err != nil {
				return nil, err
			}
			var page struct {
				Items []search.ScrapeItem `json:"items"`
				Next  string              `json:"next"`
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				return nil, fmt.Errorf("scrape plan: %w", err)
			}
			for i := range page.Items {
				if it := &page.Items[i]; !seen[it.Ino] {
					seen[it.Ino] = true
					out = append(out, *it)
				}
			}
			if page.Next == "" {
				break
			}
			cursor = page.Next
		}
	}
	return out, nil
}

// webWriter creates files through the node's web listener: the browser's exclusive
// upload (docs/13 §4, I-226), so a name that exists answers 412.
type webWriter struct {
	base string
	http *http.Client
}

func (w *webWriter) Create(ctx context.Context, p string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, w.base+"/files"+escapePath(p), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set(webui.HeaderUI, "1")
	req.Header.Set("If-None-Match", "*")
	resp, err := w.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusPreconditionFailed:
		return scrape.ErrExist
	case resp.StatusCode/100 == 2:
		return nil
	}
	var e struct {
		Error string `json:"error"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(b, &e) != nil || e.Error == "" {
		e.Error = resp.Status
	}
	return fmt.Errorf("write %s: %s", p, e.Error)
}

// escapePath escapes each segment of a volume path for a URL.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// ttyChooser lists the top results and reads a number, s or an id.
type ttyChooser struct {
	in  *bufio.Reader
	out io.Writer
}

func (t *ttyChooser) Choose(it search.ScrapeItem, cs []scrape.Candidate) (int, string, bool) {
	fmt.Fprintf(t.out, "\n%s: %q", it.Dir, it.Title)
	if it.Year != 0 {
		fmt.Fprintf(t.out, " (%d)", it.Year)
	}
	fmt.Fprintln(t.out, " has no clear match:")
	for i, c := range cs {
		line := c.Title
		if c.Artist != "" {
			line = c.Artist + " · " + line
		}
		if c.Year != 0 {
			line += fmt.Sprintf(" (%d)", c.Year)
		}
		if ov := strings.TrimSpace(c.Overview); ov != "" {
			if r := []rune(ov); len(r) > 70 {
				ov = string(r[:69]) + "…"
			}
			line += " — " + ov
		}
		fmt.Fprintf(t.out, "  %d) %s\n", i+1, line)
	}
	fmt.Fprint(t.out, "number, s to skip, or an id: ")
	line, _ := t.in.ReadString('\n')
	line = strings.TrimSpace(line)
	if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(cs) {
		return n - 1, "", false
	}
	if line == "" || line == "s" {
		return -1, "", true
	}
	return -1, line, false
}

// isTerminal reports whether f is a character device: a person at a terminal.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
