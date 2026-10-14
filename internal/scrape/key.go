package scrape

import "errors"

// Attribution is the line TMDB's terms ask for.
const Attribution = "This product uses the TMDB API but is not endorsed or certified by TMDB."

// ErrNoKey is a scrape of a keyed group with no TMDB key in the secrets store (docs/19 §6).
var ErrNoKey = errors.New("no TMDB key: run `tailraid secret set tmdb`")
