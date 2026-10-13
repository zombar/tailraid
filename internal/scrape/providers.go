package scrape

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Candidate is one search result, as matching and the chooser see it.
type Candidate struct {
	ID       string // a TMDB id, or a MusicBrainz release group's MBID
	Title    string
	Original string
	Year     int
	Artist   string // a release group's
	Overview string
}

// Meta is one item's metadata, whatever the provider: what a sidecar carries and where
// its images are.
type Meta struct {
	Root      string // movie | tvshow | episodedetails
	TMDB      int
	Title     string
	Original  string
	Year      int
	Premiered string // a film's release date, a show's first air date
	Aired     string // an episode's
	Plot      string
	Runtime   int // minutes
	Genres    []string
	Rating    float64 // 0..10, 0 none
	MPAA      string
	Directors []string
	Actors    []string // at most ten, in billing order
	Season    int      // -1 none
	Episode   int      // -1 none
	Status    string
	Poster    string // full image URLs, "" none
	Fanart    string
	Thumb     string
}

// The image sizes a scrape fetches (docs/18 §11).
const (
	posterSize = "w780"
	fanartSize = "w1280"
	stillSize  = "w300"
	maxActors  = 10
)

func (c *Client) img(size, p string) string {
	if p == "" {
		return ""
	}
	return c.Images + "/t/p/" + size + p
}

func yearOf(date string) int {
	if len(date) < 4 {
		return 0
	}
	y, _ := strconv.Atoi(date[:4])
	return y
}

func (c *Client) tmdbURL(p string, q url.Values) string {
	return c.TMDB + "/3" + p + "?" + q.Encode()
}

// CheckKey asks TMDB's configuration with the key: nil when TMDB accepts it.
func (c *Client) CheckKey(ctx context.Context) error {
	var v struct{}
	return c.getJSON(ctx, c.tmdbURL("/configuration", url.Values{}), &v)
}

// SearchMovie is TMDB's film search for title, in year when year is not 0.
func (c *Client) SearchMovie(ctx context.Context, title string, year int, lang string) ([]Candidate, error) {
	return c.searchTMDB(ctx, "/search/movie", "year", title, year, lang)
}

// SearchTV is TMDB's show search for title, first aired in year when year is not 0.
func (c *Client) SearchTV(ctx context.Context, title string, year int, lang string) ([]Candidate, error) {
	return c.searchTMDB(ctx, "/search/tv", "first_air_date_year", title, year, lang)
}

// searchTMDB asks one of TMDB's searches; a film's result names its title and release
// date, a show's its name and first air date.
func (c *Client) searchTMDB(ctx context.Context, p, yearParam, title string, year int, lang string) ([]Candidate, error) {
	q := url.Values{"query": {title}, "language": {lang}, "include_adult": {"false"}}
	if year != 0 {
		q.Set(yearParam, strconv.Itoa(year))
	}
	var v struct {
		Results []struct {
			ID           int    `json:"id"`
			Title        string `json:"title"`
			Name         string `json:"name"`
			Original     string `json:"original_title"`
			OriginalName string `json:"original_name"`
			Date         string `json:"release_date"`
			FirstAir     string `json:"first_air_date"`
			Overview     string `json:"overview"`
		} `json:"results"`
	}
	if err := c.getJSON(ctx, c.tmdbURL(p, q), &v); err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(v.Results))
	for i := range v.Results {
		r := &v.Results[i]
		out = append(out, Candidate{ID: strconv.Itoa(r.ID), Title: r.Title + r.Name, Original: r.Original + r.OriginalName, Year: yearOf(r.Date + r.FirstAir), Overview: r.Overview})
	}
	return out, nil
}

type tmdbCredits struct {
	Cast []struct {
		Name  string `json:"name"`
		Order int    `json:"order"`
	} `json:"cast"`
	Crew []struct {
		Name string `json:"name"`
		Job  string `json:"job"`
	} `json:"crew"`
}

func (cr tmdbCredits) actors() []string {
	cast := cr.Cast
	sort.SliceStable(cast, func(i, j int) bool { return cast[i].Order < cast[j].Order })
	var out []string
	for _, a := range cast {
		if len(out) == maxActors {
			break
		}
		out = append(out, a.Name)
	}
	return out
}

type tmdbGenre struct {
	Name string `json:"name"`
}

func genres(gs []tmdbGenre) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return out
}

// Movie is a film's details, its credits and its certification in region.
func (c *Client) Movie(ctx context.Context, id, lang, region string) (Meta, error) {
	var v struct {
		ID       int         `json:"id"`
		Title    string      `json:"title"`
		Original string      `json:"original_title"`
		Date     string      `json:"release_date"`
		Overview string      `json:"overview"`
		Runtime  int         `json:"runtime"`
		Genres   []tmdbGenre `json:"genres"`
		Vote     float64     `json:"vote_average"`
		Poster   string      `json:"poster_path"`
		Backdrop string      `json:"backdrop_path"`
		Credits  tmdbCredits `json:"credits"`
		Releases struct {
			Results []struct {
				Country string `json:"iso_3166_1"`
				Dates   []struct {
					Certification string `json:"certification"`
				} `json:"release_dates"`
			} `json:"results"`
		} `json:"release_dates"`
	}
	q := url.Values{"language": {lang}, "append_to_response": {"credits,release_dates"}}
	if err := c.getJSON(ctx, c.tmdbURL("/movie/"+url.PathEscape(id), q), &v); err != nil {
		return Meta{}, err
	}
	m := Meta{
		Root: "movie", TMDB: v.ID, Title: v.Title, Original: v.Original, Year: yearOf(v.Date), Premiered: v.Date, Plot: v.Overview,
		Runtime: v.Runtime, Genres: genres(v.Genres), Rating: v.Vote, Actors: v.Credits.actors(), Season: -1, Episode: -1,
		Poster: c.img(posterSize, v.Poster), Fanart: c.img(fanartSize, v.Backdrop),
	}
	for _, cr := range v.Credits.Crew {
		if cr.Job == "Director" {
			m.Directors = append(m.Directors, cr.Name)
		}
	}
	for _, r := range v.Releases.Results {
		if r.Country != region {
			continue
		}
		for _, d := range r.Dates {
			if d.Certification != "" {
				m.MPAA = d.Certification
				break
			}
		}
	}
	return m, nil
}

// Show is a show's details, its credits and its rating in region.
func (c *Client) Show(ctx context.Context, id, lang, region string) (Meta, error) {
	var v struct {
		ID       int         `json:"id"`
		Name     string      `json:"name"`
		Original string      `json:"original_name"`
		Date     string      `json:"first_air_date"`
		Overview string      `json:"overview"`
		Runtimes []int       `json:"episode_run_time"`
		Genres   []tmdbGenre `json:"genres"`
		Vote     float64     `json:"vote_average"`
		Status   string      `json:"status"`
		Poster   string      `json:"poster_path"`
		Backdrop string      `json:"backdrop_path"`
		Created  []struct {
			Name string `json:"name"`
		} `json:"created_by"`
		Credits tmdbCredits `json:"credits"`
		Ratings struct {
			Results []struct {
				Country string `json:"iso_3166_1"`
				Rating  string `json:"rating"`
			} `json:"results"`
		} `json:"content_ratings"`
	}
	q := url.Values{"language": {lang}, "append_to_response": {"credits,content_ratings"}}
	if err := c.getJSON(ctx, c.tmdbURL("/tv/"+url.PathEscape(id), q), &v); err != nil {
		return Meta{}, err
	}
	m := Meta{
		Root: "tvshow", TMDB: v.ID, Title: v.Name, Original: v.Original, Year: yearOf(v.Date), Premiered: v.Date, Plot: v.Overview,
		Genres: genres(v.Genres), Rating: v.Vote, Status: v.Status, Actors: v.Credits.actors(), Season: -1, Episode: -1,
		Poster: c.img(posterSize, v.Poster), Fanart: c.img(fanartSize, v.Backdrop),
	}
	if len(v.Runtimes) > 0 {
		m.Runtime = v.Runtimes[0]
	}
	for _, cr := range v.Created {
		m.Directors = append(m.Directors, cr.Name)
	}
	for _, r := range v.Ratings.Results {
		if r.Country == region {
			m.MPAA = r.Rating
		}
	}
	return m, nil
}

// Season is one season of a show: its poster and its episodes by number.
type Season struct {
	Poster   string
	Episodes map[int]Meta
}

// Season fetches season n of show id: one request for every episode in it.
func (c *Client) Season(ctx context.Context, id string, n int, lang string) (Season, error) {
	var v struct {
		Poster   string `json:"poster_path"`
		Episodes []struct {
			ID       int     `json:"id"`
			Number   int     `json:"episode_number"`
			Season   int     `json:"season_number"`
			Name     string  `json:"name"`
			Overview string  `json:"overview"`
			Air      string  `json:"air_date"`
			Runtime  int     `json:"runtime"`
			Still    string  `json:"still_path"`
			Vote     float64 `json:"vote_average"`
			Crew     []struct {
				Name string `json:"name"`
				Job  string `json:"job"`
			} `json:"crew"`
		} `json:"episodes"`
	}
	if err := c.getJSON(ctx, c.tmdbURL(fmt.Sprintf("/tv/%s/season/%d", url.PathEscape(id), n), url.Values{"language": {lang}}), &v); err != nil {
		return Season{}, err
	}
	s := Season{Poster: c.img(posterSize, v.Poster), Episodes: map[int]Meta{}}
	for i := range v.Episodes {
		e := &v.Episodes[i]
		m := Meta{
			Root: "episodedetails", TMDB: e.ID, Title: e.Name, Plot: e.Overview, Aired: e.Air, Year: yearOf(e.Air), Runtime: e.Runtime,
			Rating: e.Vote, Season: e.Season, Episode: e.Number, Thumb: c.img(stillSize, e.Still),
		}
		for _, cr := range e.Crew {
			if cr.Job == "Director" {
				m.Directors = append(m.Directors, cr.Name)
			}
		}
		s.Episodes[e.Number] = m
	}
	return s, nil
}

// lucene quotes a term for a MusicBrainz query.
func lucene(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// SearchAlbum is MusicBrainz's release-group search for an artist's album.
func (c *Client) SearchAlbum(ctx context.Context, artist, title string) ([]Candidate, error) {
	q := "releasegroup:" + lucene(title)
	if artist != "" {
		q += " AND artist:" + lucene(artist)
	}
	var v struct {
		Groups []struct {
			ID      string `json:"id"`
			Title   string `json:"title"`
			Date    string `json:"first-release-date"`
			Credits []struct {
				Name string `json:"name"`
			} `json:"artist-credit"`
		} `json:"release-groups"`
	}
	raw := c.MB + "/ws/2/release-group/?" + url.Values{"query": {q}, "fmt": {"json"}, "limit": {"5"}}.Encode()
	if err := c.getJSON(ctx, raw, &v); err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(v.Groups))
	for _, g := range v.Groups {
		names := make([]string, 0, len(g.Credits))
		for _, a := range g.Credits {
			names = append(names, a.Name)
		}
		out = append(out, Candidate{ID: g.ID, Title: g.Title, Year: yearOf(g.Date), Artist: strings.Join(names, " & ")})
	}
	return out, nil
}

// CoverURL is the Cover Art Archive's 500 px front image of a release group.
func (c *Client) CoverURL(mbid string) string {
	return c.CAA + "/release-group/" + url.PathEscape(mbid) + "/front-500"
}
