// Package scrape is the CLI's side of `tailraid media scrape` (docs/18 §11): the
// provider client (TMDB, MusicBrainz, the Cover Art Archive), the matching, the Kodi
// sidecars it writes and the run that ties them to a node's plan. Only the CLI imports
// it: no node ever contacts a provider (I-298).
package scrape

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/clock"
)

// The provider hosts the client speaks to; the Cover Art Archive answers with a redirect
// to archive.org, whose hosts are allowed as the archive's own.
var defaultHosts = []string{"api.themoviedb.org", "image.tmdb.org", "musicbrainz.org", "coverartarchive.org", "archive.org", ".archive.org", "thumbnails.libretro.com"}

// Bounds of a response body and of the retries (I-302).
const (
	MaxJSON    = 4 << 20
	MaxImage   = 20 << 20
	MaxListing = 16 << 20 // a libretro system's box art index (~1 MiB today)
	MaxRetries = 3
	MBInterval = time.Second // MusicBrainz asks for at most one request a second
	Timeout    = 30 * time.Second
)

var (
	// ErrNotFound is a provider's 404.
	ErrNotFound = errors.New("scrape: not found")
	// ErrKey is TMDB refusing the key (401).
	ErrKey = errors.New("scrape: TMDB refused the key (run tailraid media key)")
	// ErrHost is a request or a redirect to a host that is not a provider's.
	ErrHost = errors.New("scrape: not a provider host")
	// ErrTooLarge is a body past its bound.
	ErrTooLarge = errors.New("scrape: response too large")
)

// Client asks the providers. Its zero value is not usable: New sets the defaults; tests
// point the base URLs and Hosts at their own servers.
type Client struct {
	HTTP     *http.Client
	Clock    clock.Clock
	TMDB     string // https://api.themoviedb.org
	Images   string // https://image.tmdb.org
	MB       string // https://musicbrainz.org
	CAA      string // https://coverartarchive.org
	Libretro string // https://thumbnails.libretro.com
	Hosts    []string
	Key      string // a TMDB v3 key, or a v4 read token (a JWT, sent as a bearer token)
	UA       string

	mu     sync.Mutex
	mbNext time.Time // MusicBrainz's next allowed request
}

// New is a client with the providers' URLs, the 30 s timeout and the user agent.
func New(key, version string) *Client {
	c := &Client{
		Clock: clock.Real(), TMDB: "https://api.themoviedb.org", Images: "https://image.tmdb.org",
		MB: "https://musicbrainz.org", CAA: "https://coverartarchive.org", Libretro: "https://thumbnails.libretro.com", Hosts: defaultHosts, Key: key,
		UA: "tailraid/" + version + " (+https://github.com/zombar/tailraid)",
	}
	c.HTTP = &http.Client{Timeout: Timeout, CheckRedirect: c.checkRedirect}
	return c
}

func (c *Client) allowed(u *url.URL) bool {
	h := u.Hostname()
	for _, a := range c.Hosts {
		if h == a || strings.HasPrefix(a, ".") && strings.HasSuffix(h, a) {
			return true
		}
	}
	return false
}

func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("scrape: too many redirects")
	}
	if !c.allowed(req.URL) {
		return fmt.Errorf("%w: redirect to %s", ErrHost, req.URL.Host)
	}
	return nil
}

func (c *Client) bearer() bool { return strings.HasPrefix(c.Key, "eyJ") }

// redact removes the key from an error's text: a url.Error carries the URL.
func (c *Client) redact(err error) error {
	if err == nil || c.Key == "" || !strings.Contains(err.Error(), c.Key) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.Key, "REDACTED"))
}

// wait sleeps d on the client's clock, or until ctx ends.
func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.Clock.After(d):
		return nil
	}
}

// throttleMB holds a MusicBrainz request until a second after the one before.
func (c *Client) throttleMB(ctx context.Context) error {
	c.mu.Lock()
	now := c.Clock.Now()
	at := c.mbNext
	if at.Before(now) {
		at = now
	}
	c.mbNext = at.Add(MBInterval)
	c.mu.Unlock()
	return c.wait(ctx, at.Sub(now))
}

// get fetches raw with the user agent (the TMDB key added for a TMDB URL), retrying a 429
// after its Retry-After at most MaxRetries times, and returns the body read through limit.
func (c *Client) get(ctx context.Context, raw string, limit int64) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if !c.allowed(u) {
		return nil, fmt.Errorf("%w: %s", ErrHost, u.Host)
	}
	tmdb := strings.HasPrefix(raw, c.TMDB+"/3/") // the API, never an image or another provider
	if tmdb && !c.bearer() && c.Key != "" {
		q := u.Query()
		q.Set("api_key", c.Key)
		u.RawQuery = q.Encode()
	}
	if strings.HasPrefix(raw, c.MB+"/ws/2/") {
		if err := c.throttleMB(ctx); err != nil {
			return nil, err
		}
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
		if err != nil {
			return nil, c.redact(err)
		}
		req.Header.Set("User-Agent", c.UA)
		req.Header.Set("Accept", "application/json")
		if tmdb && c.bearer() {
			req.Header.Set("Authorization", "Bearer "+c.Key)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, c.redact(err)
		}
		body, rerr := readLimited(resp.Body, limit)
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusTooManyRequests && attempt < MaxRetries:
			secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			if err := c.wait(ctx, time.Duration(max(secs, 1))*time.Second); err != nil {
				return nil, err
			}
			continue
		case resp.StatusCode == http.StatusNotFound:
			return nil, ErrNotFound
		case resp.StatusCode == http.StatusUnauthorized && tmdb:
			return nil, ErrKey
		case resp.StatusCode/100 != 2:
			return nil, fmt.Errorf("scrape: %s answered %s", u.Host, resp.Status)
		}
		return body, rerr
	}
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, ErrTooLarge
	}
	return b, nil
}

func (c *Client) getJSON(ctx context.Context, raw string, v any) error {
	b, err := c.get(ctx, raw, MaxJSON)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("scrape: decode %s: %w", strings.SplitN(raw, "?", 2)[0], err)
	}
	return nil
}

// Image fetches an image the providers serve (a TMDB image URL, a Cover Art Archive one).
func (c *Client) Image(ctx context.Context, raw string) ([]byte, error) {
	return c.get(ctx, raw, MaxImage)
}
