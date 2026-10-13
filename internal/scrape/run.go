package scrape

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/zombar/tailraid/internal/search"
)

// ErrExist is a Writer's answer for a name that exists: the slot is present (I-299).
var ErrExist = errors.New("scrape: exists")

// Writer creates a file in the volume, exclusively: ErrExist when the name exists.
type Writer interface {
	Create(ctx context.Context, path string, data []byte) error
}

// Chooser asks the user to pick among candidates that are not a clear match: the index
// taken, or an id typed in, or skip. A nil Chooser is no terminal: the item is skipped.
type Chooser interface {
	Choose(it search.ScrapeItem, cs []Candidate) (pick int, id string, skip bool)
}

// The --only groups.
const (
	OnlyMovies   = "movies"
	OnlyTV       = "tv"
	OnlyEpisodes = "episodes"
	OnlyMusic    = "music"
	OnlyGames    = "games"
)

// Options are a run's flags.
type Options struct {
	Only   map[string]bool // empty: every group
	Lang   string
	Region string
	Yes    bool   // take the top result of an unclear search
	DryRun bool   // write nothing, print what would be written
	ID     string // the id of the one film, show, album or game the paths name (tmdb:<n>, mbid:<uuid> or libretro:<name>)
	Out    io.Writer
}

// Skip is an item a run left alone, and why.
type Skip struct {
	Path, Why string
}

// Summary is what a run did.
type Summary struct {
	Items   int // items something was written for (or would be, dry)
	Files   int
	Present int // slots found present when written (a 412)
	Skipped []Skip
}

type runner struct {
	c   *Client
	w   Writer
	ch  Chooser
	o   Options
	sum Summary
	// a show's match and its seasons, by the show's inode
	shows   map[uint64]string
	seasons map[string]Season
	boxarts map[string][]string // a libretro system's names, fetched once a run
}

func (o Options) wants(group string) bool { return len(o.Only) == 0 || o.Only[group] }

// keyless are the groups whose providers need no key: MusicBrainz with the Cover Art
// Archive, and the libretro thumbnails.
var keyless = map[string]bool{OnlyMusic: true, OnlyGames: true}

// WithoutKey narrows only (empty: every group) to the groups a run can do with no TMDB
// key, and names the ones it dropped; an only that names nothing but TMDB groups is an
// error, ErrNoKey.
func WithoutKey(only map[string]bool) (map[string]bool, []string, error) {
	if len(only) == 0 {
		return map[string]bool{OnlyMusic: true, OnlyGames: true}, []string{OnlyMovies, OnlyTV, OnlyEpisodes}, nil
	}
	out := map[string]bool{}
	var dropped []string
	for _, g := range []string{OnlyMovies, OnlyTV, OnlyEpisodes, OnlyMusic, OnlyGames} {
		switch {
		case !only[g]:
		case keyless[g]:
			out[g] = true
		default:
			dropped = append(dropped, g)
		}
	}
	if len(out) == 0 {
		return nil, dropped, ErrNoKey
	}
	return out, dropped, nil
}

// Run scrapes the plan's items (docs/18 §11): each film, show and album matched (I-301),
// then each missing slot written with its provider's data, exclusively (I-299).
func Run(ctx context.Context, c *Client, items []search.ScrapeItem, w Writer, ch Chooser, o Options) (Summary, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	r := &runner{c: c, w: w, ch: ch, o: o, shows: map[uint64]string{}, seasons: map[string]Season{}, boxarts: map[string][]string{}}
	if o.ID != "" {
		roots := 0
		for i := range items {
			if it := &items[i]; it.Kind == search.ScrapeMovie || it.Kind == search.ScrapeShow || it.Kind == search.ScrapeAlbum || it.Kind == search.ScrapeGame {
				roots++
			}
		}
		if roots != 1 {
			return r.sum, fmt.Errorf("scrape: --id names one item, the paths hold %d films, shows, albums or games", roots)
		}
	}
	for i := range items {
		it := items[i]
		if err := ctx.Err(); err != nil {
			return r.sum, err
		}
		var err error
		switch it.Kind {
		case search.ScrapeMovie:
			if o.wants(OnlyMovies) {
				err = r.movie(ctx, it)
			}
		case search.ScrapeShow:
			if o.wants(OnlyTV) || o.wants(OnlyEpisodes) {
				err = r.show(ctx, it)
			}
		case search.ScrapeSeason:
			if o.wants(OnlyTV) {
				err = r.season(ctx, it)
			}
		case search.ScrapeEpisode:
			if o.wants(OnlyEpisodes) {
				err = r.episode(ctx, it)
			}
		case search.ScrapeAlbum:
			if o.wants(OnlyMusic) {
				err = r.album(ctx, it)
			}
		case search.ScrapeGame:
			if o.wants(OnlyGames) {
				err = r.game(ctx, it)
			}
		}
		if errors.Is(err, ErrKey) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return r.sum, err
		}
		if err != nil {
			r.skip(it, err.Error())
		}
	}
	return r.sum, nil
}

func (r *runner) skip(it search.ScrapeItem, why string) {
	r.sum.Skipped = append(r.sum.Skipped, Skip{Path: itemPath(it), Why: why})
}

func itemPath(it search.ScrapeItem) string {
	switch it.Kind {
	case search.ScrapeSeason:
		return fmt.Sprintf("%s (season %d)", it.Dir, it.Season)
	case search.ScrapeEpisode:
		return fmt.Sprintf("%s (S%02dE%02d)", it.Dir, it.Season, it.Episode)
	}
	return it.Dir + " (" + it.Title + ")"
}

// pick resolves an item to one candidate's id: --id, a clear match, --yes, the chooser,
// or "" with the reason it was skipped.
func (r *runner) pick(it search.ScrapeItem, scheme string, cs []Candidate, isClear func() (int, bool)) (string, string) {
	if r.o.ID != "" {
		if id, ok := strings.CutPrefix(r.o.ID, scheme+":"); ok {
			return id, ""
		}
		return "", "--id is not a " + scheme + " id"
	}
	if len(cs) == 0 {
		return "", "not found"
	}
	if i, ok := isClear(); ok {
		return cs[i].ID, ""
	}
	if r.o.Yes {
		return cs[0].ID, ""
	}
	if r.ch == nil {
		return "", fmt.Sprintf("no clear match among %d results (run in a terminal to choose, or pass --yes or --id)", len(cs))
	}
	top := cs[:min(5, len(cs))]
	i, id, skip := r.ch.Choose(it, top)
	switch {
	case skip:
		return "", "skipped"
	case id != "":
		return id, ""
	case i >= 0 && i < len(top):
		return top[i].ID, ""
	}
	return "", "skipped"
}

// search asks with the year, and again without it when the year finds nothing (a name's
// year is often a release's, a year off).
func (r *runner) search(ctx context.Context, it search.ScrapeItem, find func(context.Context, string, int, string) ([]Candidate, error)) ([]Candidate, error) {
	cs, err := find(ctx, it.Title, it.Year, r.o.Lang)
	if err == nil && len(cs) == 0 && it.Year != 0 {
		cs, err = find(ctx, it.Title, 0, r.o.Lang)
	}
	return cs, err
}

func (r *runner) movie(ctx context.Context, it search.ScrapeItem) error {
	cs, err := r.search(ctx, it, r.c.SearchMovie)
	if err != nil {
		return err
	}
	id, why := r.pick(it, "tmdb", cs, func() (int, bool) { return Clear(it.Title, it.Year, cs) })
	if id == "" {
		r.skip(it, why)
		return nil
	}
	m, err := r.c.Movie(ctx, id, r.o.Lang, r.o.Region)
	if err != nil {
		return err
	}
	return r.write(ctx, it, map[string]func() ([]byte, error){
		"nfo":    func() ([]byte, error) { return NFO(m), nil },
		"poster": r.image(ctx, m.Poster),
		"fanart": r.image(ctx, m.Fanart),
	})
}

func (r *runner) show(ctx context.Context, it search.ScrapeItem) error {
	cs, err := r.search(ctx, it, r.c.SearchTV)
	if err != nil {
		return err
	}
	id, why := r.pick(it, "tmdb", cs, func() (int, bool) { return Clear(it.Title, it.Year, cs) })
	if id == "" {
		r.skip(it, why)
		return nil
	}
	r.shows[it.Ino] = id
	if len(it.Missing) == 0 || !r.o.wants(OnlyTV) {
		return nil
	}
	m, err := r.c.Show(ctx, id, r.o.Lang, r.o.Region)
	if err != nil {
		return err
	}
	return r.write(ctx, it, map[string]func() ([]byte, error){
		"nfo":    func() ([]byte, error) { return NFO(m), nil },
		"poster": r.image(ctx, m.Poster),
		"fanart": r.image(ctx, m.Fanart),
	})
}

// seasonOf is a show's season n, fetched once a run.
func (r *runner) seasonOf(ctx context.Context, it search.ScrapeItem) (Season, error) {
	id, ok := r.shows[it.Show]
	if !ok {
		return Season{}, errors.New("its show was not matched")
	}
	key := fmt.Sprintf("%s/%d", id, it.Season)
	if s, ok := r.seasons[key]; ok {
		return s, nil
	}
	s, err := r.c.Season(ctx, id, it.Season, r.o.Lang)
	if err != nil {
		return Season{}, err
	}
	r.seasons[key] = s
	return s, nil
}

func (r *runner) season(ctx context.Context, it search.ScrapeItem) error {
	s, err := r.seasonOf(ctx, it)
	if err != nil {
		return err
	}
	return r.write(ctx, it, map[string]func() ([]byte, error){"poster": r.image(ctx, s.Poster)})
}

func (r *runner) episode(ctx context.Context, it search.ScrapeItem) error {
	s, err := r.seasonOf(ctx, it)
	if err != nil {
		return err
	}
	m, ok := s.Episodes[it.Episode]
	if !ok {
		r.skip(it, "the provider lists no such episode")
		return nil
	}
	return r.write(ctx, it, map[string]func() ([]byte, error){
		"nfo":   func() ([]byte, error) { return NFO(m), nil },
		"thumb": r.image(ctx, m.Thumb),
	})
}

func (r *runner) album(ctx context.Context, it search.ScrapeItem) error {
	cs, err := r.c.SearchAlbum(ctx, it.Artist, it.Title)
	if err != nil {
		return err
	}
	id, why := r.pick(it, "mbid", cs, func() (int, bool) { return ClearAlbum(it.Artist, it.Title, cs) })
	if id == "" {
		r.skip(it, why)
		return nil
	}
	return r.write(ctx, it, map[string]func() ([]byte, error){"cover": r.image(ctx, r.c.CoverURL(id))})
}

func (r *runner) game(ctx context.Context, it search.ScrapeItem) error {
	names, ok := r.boxarts[it.System]
	if !ok {
		var err error
		if names, err = r.c.Boxarts(ctx, it.System); err != nil {
			return err
		}
		r.boxarts[it.System] = names
	}
	cs, idx, isClear := ClearGame(it.Key, it.Title, it.Regions, names)
	name, why := r.pick(it, "libretro", cs, func() (int, bool) { return idx, isClear })
	if name == "" {
		r.skip(it, why)
		return nil
	}
	return r.write(ctx, it, map[string]func() ([]byte, error){"boxart": r.image(ctx, r.c.BoxartURL(it.System, name))})
}

// errNoImage is a provider with no image for a slot: the slot stays missing.
var errNoImage = errors.New("no image")

func (r *runner) image(ctx context.Context, raw string) func() ([]byte, error) {
	return func() ([]byte, error) {
		if raw == "" {
			return nil, errNoImage
		}
		b, err := r.c.Image(ctx, raw)
		if errors.Is(err, ErrNotFound) {
			return nil, errNoImage
		}
		return b, err
	}
}

// write writes the item's missing slots that have data, each as an exclusive create.
func (r *runner) write(ctx context.Context, it search.ScrapeItem, data map[string]func() ([]byte, error)) error {
	wrote := 0
	for _, s := range it.Missing {
		get, ok := data[s.Slot]
		if !ok {
			continue
		}
		p := path.Join(it.Dir, s.Name)
		if r.o.DryRun {
			fmt.Fprintln(r.o.Out, "would write", p)
			wrote++
			continue
		}
		b, err := get()
		if errors.Is(err, errNoImage) {
			continue
		}
		if err != nil {
			return err
		}
		switch err := r.w.Create(ctx, p, b); {
		case errors.Is(err, ErrExist):
			r.sum.Present++
		case err != nil:
			return err
		default:
			fmt.Fprintln(r.o.Out, "wrote", p)
			wrote++
		}
	}
	if wrote > 0 {
		r.sum.Items++
		r.sum.Files += wrote
	}
	return nil
}
