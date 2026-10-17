package soak

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/search"
	"github.com/zombar/tailraid/internal/webui"
)

// --- 35 watching and media search -------------------------------------------------------
//
// docs/13 §10c, §10d and docs/18 §6a on a running cluster. A films and a shows library are
// written through the mount with sidecars naming people and genres, and flagged. A
// position reported through one gateway reads back on every gateway and puts the film in
// Continue watching (a); a position past the watched mark ticks it and drops it from the
// row (b); the last episode of a season watched puts the next season's first in the row
// as next up (c); a season marked watched and unwatched lands on every gateway (d); a
// second position within the coalescing window is dropped and a final one is not (e);
// the film purged takes its row with it (f); a table of media queries answers the same
// items on every gateway in both scopes, a malformed one a 400 with its offset (g), and
// the suggestions for a prefix name the sidecar's actor (h). Everything is deleted and
// purged after, and the flags cleared.

// watchQuery is an event: one media query's answer time per gateway, for the report.
const watchQuery = "media query"

func phaseWatch(ctx context.Context, r *Run) error {
	var clients []*webClient
	var names []string
	hc := r.webClientFor()
	for _, n := range r.gateways() {
		base, err := r.Env.WebURL(ctx, n)
		if err != nil || base == "" {
			continue
		}
		clients = append(clients, &webClient{base: trimSlash(base), c: hc})
		names = append(names, n.Name)
	}
	if len(clients) == 0 {
		r.skip("no browser UI in this environment")
		return nil
	}
	if !r.Env.Caps().Mutate {
		r.skip("playback rows and library flags change the volume: needs --destructive")
		return nil
	}
	w := clients[0]
	gw := r.gateways()[0]
	v := r.volume(gw)
	u := r.undoer(ctx)
	defer u.run()
	dir := join(r.Root, "watch")
	tag := hex.EncodeToString(r.randomBytes(3))
	actor := "Soak Actor " + tag
	films, shows := join(dir, "films"), join(dir, "shows")
	film := join(films, "Probe "+tag+" (1995)")
	other := join(films, "Other "+tag+" (2001)")
	show := join(shows, "Show "+tag)
	ep := func(s, e int) string {
		return join(show, fmt.Sprintf("Season %d", s), fmt.Sprintf("Show.%s.S%02dE%02d.mkv", tag, s, e))
	}
	m := model{
		join(film, "probe.mkv"):  r.randomBytes(4096),
		join(film, "movie.nfo"):  []byte(`<movie><title>Probe ` + tag + `</title><year>1995</year><genre>Thriller</genre><director>Soak Director ` + tag + `</director><actor><name>` + actor + `</name></actor><plot>A heist ` + tag + `.</plot></movie>`),
		join(other, "other.mkv"): r.randomBytes(4096),
		join(other, "movie.nfo"): []byte(`<movie><title>Other ` + tag + `</title><year>2001</year><genre>Drama</genre><actor><name>` + actor + ` Jr</name></actor></movie>`),
		join(show, "tvshow.nfo"): []byte(`<tvshow><title>Show ` + tag + `</title><year>2010</year><genre>Drama</genre><actor><name>` + actor + `</name></actor></tvshow>`),
		ep(1, 1):                 r.randomBytes(2048),
		ep(1, 2):                 r.randomBytes(2048),
		ep(2, 1):                 r.randomBytes(2048),
	}
	if err := r.writeModel(v, m); err != nil {
		r.checkAt("I-405", "the phase's files written", false, err.Error(), "", "", gw.Name)
		return nil
	}
	if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
		return err
	}
	u.add("delete and purge the phase's folder", func(c context.Context) error {
		if _, _, err := w.del(c, dir); err != nil {
			return err
		}
		_, err := r.Env.PurgeTrash(c)
		return err
	})
	for p, kind := range map[string]string{films: "movies", shows: "tv"} {
		if code, body, _ := w.postJSON(ctx, "/api/flags", map[string]any{"path": p, "library": kind}, nil); code != 200 {
			r.check("I-210", "flag "+p+" as "+kind, false, fmt.Sprintf("%d %s", code, body))
			return nil
		}
	}
	for range 10 {
		res, err := r.Env.IndexNow(ctx)
		if err != nil || res.Pending == 0 {
			break
		}
	}
	if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
		return err
	}
	// eventually polls every gateway until ok holds of each answer, for the replication
	// of a proposal to every node
	eventually := func(ok func(c *webClient) bool) (bool, string) {
		for range 15 {
			bad := ""
			for i, c := range clients {
				if !ok(c) {
					bad = names[i]
					break
				}
			}
			if bad == "" {
				return true, ""
			}
			if err := r.Env.Wait(ctx, time.Second); err != nil {
				return false, err.Error()
			}
		}
		return false, "not on every gateway"
	}
	report := func(c *webClient, body map[string]any) (int, string) {
		code, b, _ := c.postJSON(ctx, "/api/playback", body, nil)
		return code, string(b)
	}
	progress := func(c *webClient, p string) (int, webui.Progress) {
		var pr webui.Progress
		code, _ := c.getJSON(ctx, "/api/playback", url.Values{"path": {p}}, &pr)
		return code, pr
	}
	contRow := func(c *webClient) ([]webui.MediaItem, []byte) {
		var out struct {
			Items []webui.MediaItem `json:"items"`
		}
		_, b, _ := c.do(ctx, http.MethodGet, "/api/media", url.Values{"continue": {"1"}}, nil, "")
		_ = json.Unmarshal(b, &out)
		return out.Items, b
	}
	inRow := func(c *webClient, p string, nextUp bool) bool {
		items, _ := contRow(c)
		for i := range items {
			if (items[i].Path == p || items[i].Video == p) && items[i].NextUp == nextUp {
				return true
			}
		}
		return false
	}
	const minute = 60_000

	// (a) a position through one gateway reads back on every gateway and is in the row
	filmVideo := join(film, "probe.mkv")
	code, body := report(w, map[string]any{"path": filmVideo, "pos": 30 * minute, "dur": 116 * minute, "final": true})
	if code == http.StatusForbidden {
		r.checkSkipped("I-407 I-408 I-409 I-410", "playback through the web UI", "the driver's requests have no viewer here (no signed-in user, no Tailscale login): "+body)
	} else {
		r.check("I-407", "a position is accepted for the request's viewer", code == http.StatusNoContent, fmt.Sprintf("%d %s", code, body))
		ok, detail := eventually(func(c *webClient) bool {
			code, pr := progress(c, filmVideo)
			return code == 200 && pr.Pos == 30*minute && pr.Dur == 116*minute && !pr.Watched
		})
		r.check("I-405 I-407", "the position reads back on every gateway", ok, detail)
		ok, detail = eventually(func(c *webClient) bool { return inRow(c, film, false) })
		r.check("I-409", "the started film is in Continue watching on every gateway", ok, detail)
		_, want := contRow(w)
		same := true
		for i, c := range clients[1:] {
			if _, got := contRow(c); !bytes.Equal(got, want) {
				same = false
				r.checkAt("I-409", "the same row on "+names[i+1], false, string(got), "", "", names[i+1])
			}
		}
		r.check("I-409", "every gateway answers the row with the same bytes", same, fmt.Sprintf("%d gateways", len(clients)))

		// (b) past the watched mark: ticked, out of the row
		report(w, map[string]any{"path": filmVideo, "pos": 112 * minute, "dur": 116 * minute, "final": true})
		ok, detail = eventually(func(c *webClient) bool {
			code, pr := progress(c, filmVideo)
			return code == 200 && pr.Watched && !inRow(c, film, false)
		})
		r.check("I-405 I-409", "a position past the mark is watched and leaves the row", ok, detail)

		// (c) the last episode of season 1 watched: season 2's first is next up
		report(w, map[string]any{"path": ep(1, 2), "pos": 45 * minute, "dur": 45 * minute, "final": true})
		ok, detail = eventually(func(c *webClient) bool { return inRow(c, ep(2, 1), true) })
		r.check("I-409", "the next season's first episode is next up", ok, detail)

		// (d) a season marked watched and unwatched lands on every gateway
		season := func(watched bool) bool {
			action := "unwatched"
			if watched {
				action = "watched"
			}
			code, b := report(w, map[string]any{"paths": []string{ep(1, 1), ep(1, 2), ep(2, 1)}, "action": action})
			if code != 200 {
				r.check("I-410", "mark "+action, false, fmt.Sprintf("%d %s", code, b))
				return false
			}
			ok, detail := eventually(func(c *webClient) bool {
				var lib struct {
					Items []webui.MediaItem `json:"items"`
				}
				code, _ := c.getJSON(ctx, "/api/media", url.Values{"lib": {shows}}, &lib)
				return code == 200 && len(lib.Items) == 1 && lib.Items[0].Watched == watched
			})
			r.check("I-410", "the show is "+action+" on every gateway", ok, detail)
			return ok
		}
		if season(true) {
			season(false)
		}

		// (e) the window: a second position at once is dropped, a final one is not
		otherVideo := join(other, "other.mkv")
		report(w, map[string]any{"path": otherVideo, "pos": 5 * minute, "dur": 100 * minute})
		report(w, map[string]any{"path": otherVideo, "pos": 6 * minute, "dur": 100 * minute})
		_, first := progress(w, otherVideo)
		report(w, map[string]any{"path": otherVideo, "pos": 7 * minute, "dur": 100 * minute, "final": true})
		ok, detail = eventually(func(c *webClient) bool { _, pr := progress(c, otherVideo); return pr.Pos == 7*minute })
		r.check("I-408", "a position within the window is dropped and a final one lands", first.Pos == 5*minute && ok, fmt.Sprintf("after two: %d; %s", first.Pos, detail))

		// (f) the film purged takes its row
		if _, code, _ := w.del(ctx, other); code != 200 {
			r.check("I-405", "delete the second film", false, strconv.Itoa(code))
		} else if _, err := r.Env.PurgeTrash(ctx); err != nil {
			r.check("I-405", "purge the trash", false, err.Error())
		} else {
			ok, detail = eventually(func(c *webClient) bool { return !inRow(c, other, false) })
			r.check("I-405 I-409", "a purged film leaves no row", ok, detail)
		}
	}

	// (g) the media query table, the same on every gateway, both scopes matching the same items
	type qcase struct {
		q    string
		want []string // titles in scope media
		bad  bool
	}
	cases := []qcase{
		{`actor:"` + actor + `"`, []string{"Probe " + tag, "Show " + tag}, false},
		{`director:"Soak Director ` + tag + `"`, []string{"Probe " + tag}, false},
		{`genre:thriller year:199x`, nil, false}, // every thriller of the 1990s, ours among them
		{`heist ` + tag, []string{"Probe " + tag}, false},
		{`actor:"` + actor, nil, true},
	}
	for _, qc := range cases {
		var first []byte
		times := map[string]string{}
		same := true
		var hits struct {
			Libs []webui.MediaHits `json:"libs"`
		}
		for i, c := range clients {
			t0 := time.Now()
			code, b, _ := c.do(ctx, http.MethodGet, "/api/search", url.Values{"scope": {"media"}, "q": {qc.q}}, nil, "")
			times[names[i]] = strconv.FormatInt(time.Since(t0).Milliseconds(), 10)
			if qc.bad {
				var e struct {
					At *int `json:"at"`
				}
				_ = json.Unmarshal(b, &e)
				if code != 400 || e.At == nil {
					same = false
					r.checkAt("I-411", "a malformed query is a 400 with its offset on "+names[i], false, fmt.Sprintf("%d %s", code, b), "", "", names[i])
				}
				continue
			}
			if i == 0 {
				first = b
				_ = json.Unmarshal(b, &hits)
			} else if !bytes.Equal(b, first) {
				same = false
				r.checkAt("I-412", "the same hits on "+names[i], false, string(b), "", "", names[i])
			}
		}
		r.emit(Record{Kind: "event", Name: watchQuery, Text: qc.q, Attrs: times})
		if qc.bad {
			r.check("I-411", "a malformed query is a 400 with its offset on every gateway", same, qc.q)
			continue
		}
		var titles []string
		for _, l := range hits.Libs {
			for j := range l.Items {
				if t := l.Items[j].Title; len(qc.want) == 0 || slices.Contains(qc.want, t) || t == "Probe "+tag {
					titles = append(titles, t)
				}
			}
		}
		slices.Sort(titles)
		want := slices.Clone(qc.want)
		if want == nil {
			want = []string{"Probe " + tag}
		}
		slices.Sort(want)
		found := true
		for _, t := range want {
			found = found && slices.Contains(titles, t)
		}
		r.check("I-412", "query "+qc.q+" answers its items on every gateway", same && found, fmt.Sprintf("%v", titles))
		// the files scope names files of the same items
		var files search.Result
		code, _ := w.getJSON(ctx, "/api/search", url.Values{"q": {qc.q}, "under": {dir}}, &files)
		hasFile := false
		for j := range files.Hits {
			hasFile = hasFile || files.Hits[j].Path == filmVideo
		}
		if strings.Contains(qc.q, ":") && slices.Contains(want, "Probe "+tag) { // a field term names its items' files; words match names
			r.check("I-412", "query "+qc.q+" in the files scope names the film's video", code == 200 && hasFile, fmt.Sprintf("%d %+v", code, files.Hits))
		}
	}

	// (h) suggestions name the sidecar's actor for a prefix of the run's tag
	var sug struct {
		Groups []search.SuggestGroup `json:"groups"`
	}
	code, _ = w.getJSON(ctx, "/api/search/suggest", url.Values{"scope": {"media"}, "q": {"soak"}}, &sug)
	named := false
	for _, g := range sug.Groups {
		for _, val := range g.Values {
			named = named || (g.Field == "actor" && val.Value == actor)
		}
	}
	r.check("I-413", "the suggestions name the actor for a prefix", code == 200 && named, fmt.Sprintf("%d %+v", code, sug.Groups))
	for _, p := range []string{films, shows} {
		u.add("clear the library flag of "+p, func(c context.Context) error {
			_, _, err := w.postJSON(c, "/api/flags", map[string]any{"path": p, "library": "none"}, nil)
			return err
		})
	}
	return nil
}

func trimSlash(s string) string { return strings.TrimRight(s, "/") }
