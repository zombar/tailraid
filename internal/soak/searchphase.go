package soak

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/search"
	"github.com/zombar/tailraid/internal/webui"
)

// --- 21 search and media ---------------------------------------------------------------
//
// docs/18 on a running cluster. Text files carrying a word made for this run, a tagged
// track, a Kodi-style film folder and a show are written through the mount; with
// --destructive their folders are flagged as libraries. After the leader's pass every
// node finds the word by content and by tags and answers the same bytes (a); a file
// renamed through the mount is found by its new name with no pass and a trashed one is
// gone at once (b); a content hit's offset names the word's bytes in the file (c); the
// markdown file renders with only the renderer's tags (d); the catalogue gives the film
// its plot and poster and the show its episode, the same on every node (e). Everything
// is deleted and purged after, and the flags cleared.

var (
	renderTag    = regexp.MustCompile(`<(/?)([a-z0-9]+)`)
	renderOwnTag = map[string]bool{"h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "p": true, "em": true, "strong": true, "del": true, "code": true, "pre": true, "blockquote": true, "ul": true, "ol": true, "li": true, "input": true, "table": true, "thead": true, "tbody": true, "tr": true, "th": true, "td": true, "hr": true, "br": true, "a": true, "img": true}
)

type searchAnswer struct {
	Query string       `json:"query"`
	Hits  []search.Hit `json:"hits"`
	Index string       `json:"index"`
}

func (a searchAnswer) has(match, p string) bool {
	for i := range a.Hits {
		if a.Hits[i].Match == match && a.Hits[i].Path == p {
			return true
		}
	}
	return false
}

// rangeGet reads bytes [from, to] of a file through the web UI.
func (w *webClient) rangeGet(ctx context.Context, p string, from, to int) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.base+"/files"+encodeWebPath(p), http.NoBody)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))
	resp, err := w.c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func phaseSearch(ctx context.Context, r *Run) error {
	var clients []*webClient
	var names []string
	hc := r.webClientFor()
	for _, n := range r.gateways() {
		base, err := r.Env.WebURL(ctx, n)
		if err != nil || base == "" {
			continue
		}
		clients = append(clients, &webClient{base: strings.TrimSuffix(base, "/"), c: hc})
		names = append(names, n.Name)
	}
	if len(clients) == 0 {
		r.skip("no browser UI in this environment")
		return nil
	}
	w := clients[0]
	gw := r.gateways()[0]
	v := r.volume(gw)
	caps := r.Env.Caps()
	u := r.undoer(ctx)
	defer u.run()
	dir := join(r.Root, "search")
	word := "zq" + hex.EncodeToString(r.randomBytes(4))
	md := join(dir, "notes", "readme.md")
	mdBody := []byte("# Notes " + word + "\n\nThe word " + word + " lives here; see [the other](other.txt).\n\n<script>alert(1)</script>\n\n| a | b |\n|---|--:|\n| 1 | 2 |\n")
	other := join(dir, "notes", "other.txt")
	otherBody := []byte("padding padding padding " + word + " and more\n")
	track := join(dir, "music", "Artist", "Album", "01 track.mp3")
	film := join(dir, "films", "Probe (2001)")
	show := join(dir, "shows", "Probe Show")
	snes := join(dir, "roms", "SNES")
	game := join(snes, "USA", "Probe Game (USA) (Rev 1).zip")
	m := model{
		md:    mdBody,
		other: otherBody,
		track: id3v2(map[string]string{"TIT2": "Song " + word, "TPE1": "Soak Artist", "TALB": "Soak Album"}, nil),
		join(dir, "music", "Artist", "Album", "02 track.mp3"): id3v2(map[string]string{"TIT2": "Second", "TPE1": "Soak Artist", "TALB": "Soak Album"}, nil),
		join(film, "probe.mkv"):                               r.randomBytes(4096),
		join(film, "movie.nfo"):                               []byte("<movie><title>Probe</title><year>2001</year><plot>A probe " + word + ".</plot></movie>"),
		join(film, "poster.jpg"):                              solidJPEG(40, 60, color.RGBA{60, 80, 100, 255}),
		join(show, "Season 1", "Probe.Show.S01E02.mkv"):       r.randomBytes(2048),
		game: r.randomBytes(1024),
		join(snes, "USA", "Probe Game (USA).zip"): r.randomBytes(1024),
		join(snes, "notes.txt"):                   []byte("not a game"),
		join(snes, "gamelist.xml"):                []byte("<gameList><game><path>./USA/Probe Game (USA) (Rev 1).zip</path><name>Probe Game</name><desc>A probe " + word + ".</desc></game></gameList>"),
	}
	if err := r.writeModel(v, m); err != nil {
		r.checkAt("I-207", "the phase's files written", false, err.Error(), "", "", gw.Name)
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
	flagged := false
	if caps.Mutate {
		ok := true
		for p, kind := range map[string]string{join(dir, "films"): "movies", join(dir, "shows"): "tv", join(dir, "music"): "music", join(dir, "roms"): "roms"} {
			code, body, _ := w.postJSON(ctx, "/api/flags", map[string]any{"path": p, "library": kind}, nil)
			if code != 200 {
				ok = false
				r.check("I-210", "flag "+p+" as "+kind, false, fmt.Sprintf("%d %s", code, body))
			}
		}
		flagged = ok
	} else {
		r.checkSkipped("I-211", "the catalogue of flagged folders", "library flags change the volume: needs --destructive")
	}

	// the leader's pass, until nothing is pending
	var res search.PassResult
	var err error
	for range 10 {
		if res, err = r.Env.IndexNow(ctx); err != nil || res.Pending == 0 {
			break
		}
	}
	r.check("I-206 I-207", "the leader's pass indexes the phase's text and tags", err == nil && res.Indexed >= 3 && res.Pending == 0, fmt.Sprintf("%+v %v", res, err))
	if err != nil {
		return nil
	}
	if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
		return err
	}

	// (a) every node finds the word by content and tags, with the same bytes
	query := func(c *webClient, q string) (searchAnswer, []byte) {
		var a searchAnswer
		code, body, _ := c.do(ctx, http.MethodGet, "/api/search", url.Values{"q": {q}, "under": {dir}}, nil, "")
		if code == 200 {
			_ = json.Unmarshal(body, &a)
		}
		return a, body
	}
	first, firstBody := query(w, word)
	r.check("I-208", "the word is found by content and by tags", first.has("content", md) && first.has("content", other) && first.has("tags", track) && first.Index == "", fmt.Sprintf("%+v", first.Hits))
	same := true
	for i, c := range clients[1:] {
		var body []byte
		for range 10 { // a follower applies the segment's commit a moment after the leader
			if _, body = query(c, word); bytes.Equal(body, firstBody) {
				break
			}
			if err := r.Env.Wait(ctx, time.Second); err != nil {
				return err
			}
		}
		if !bytes.Equal(body, firstBody) {
			same = false
			r.checkAt("I-208", "the same answer on "+names[i+1], false, string(body), "", "", names[i+1])
		}
	}
	r.check("I-208", "every gateway answers the same bytes", same, fmt.Sprintf("%d gateways", len(clients)))

	// (b) a rename is found by its new name at once; a trashed file is gone at once
	renamed := join(dir, "notes", word+" renamed.txt")
	if err := v.Rename(other, renamed); err != nil {
		r.check("I-208", "rename through the mount", false, err.Error())
		return nil
	}
	if err := r.Env.Wait(ctx, time.Second); err != nil { // replication to the answering node: its names follow the feed at once (I-388)
		return err
	}
	after, _ := query(w, word+" renamed")
	r.check("I-208", "a renamed file is found by its new name with no pass", after.has("name", renamed), fmt.Sprintf("%+v", after.Hits))
	if _, code, _ := w.del(ctx, renamed); code != 200 {
		r.check("I-208", "delete through the web", false, fmt.Sprint(code))
	}
	if err := r.Env.Wait(ctx, time.Second); err != nil { // replication to the answering node: its names follow the feed at once (I-388)
		return err
	}
	gone, _ := query(w, word)
	trashed := false
	for i := range gone.Hits {
		trashed = trashed || strings.HasPrefix(gone.Hits[i].Path, renamed)
	}
	r.check("I-208", "a trashed file is no hit", !trashed && gone.has("content", md), fmt.Sprintf("%+v", gone.Hits))

	// (c) a content hit's offset names the word's bytes
	snipOK, detail := false, "no content hit"
	for i := range first.Hits {
		h := &first.Hits[i]
		if h.Path != md || h.Match != "content" {
			continue
		}
		for _, t := range h.Terms {
			code, b, _ := w.rangeGet(ctx, md, t.Off, t.Off+len(word)-1)
			snipOK = (code == 206 || code == 200) && string(b) == word && bytes.Equal(mdBody[t.Off:t.Off+len(word)], b)
			detail = fmt.Sprintf("%d %q at %d", code, b, t.Off)
		}
	}
	r.check("I-208", "a content hit's offset reads the word through a Range request", snipOK, detail)

	// (d) the markdown renders with only the renderer's tags
	code, html, _ := w.do(ctx, http.MethodGet, "/api/render", url.Values{"path": {md}}, nil, "")
	own := code == 200
	for _, mt := range renderTag.FindAllStringSubmatch(string(html), -1) {
		own = own && renderOwnTag[mt[2]]
	}
	r.check("I-209", "the markdown renders with only the renderer's tags, the script as text", own && strings.Contains(string(html), "&lt;script&gt;") && strings.Contains(string(html), `align="right"`), fmt.Sprintf("%d %.200s", code, html))

	// (e) the catalogue, the same on every node
	if !flagged {
		return nil
	}
	var lib struct {
		Items []webui.MediaItem `json:"items"`
	}
	code, _ = w.getJSON(ctx, "/api/media", url.Values{"lib": {join(dir, "films")}}, &lib)
	filmOK := code == 200 && len(lib.Items) == 1 && lib.Items[0].Title == "Probe" && lib.Items[0].Year == 2001 && lib.Items[0].Poster == join(film, "poster.jpg") && strings.Contains(lib.Items[0].Plot, word)
	r.check("I-211 I-212", "the film carries its sidecar's plot and poster", filmOK, fmt.Sprintf("%d %+v", code, lib.Items))
	var it webui.MediaItem
	code, _ = w.getJSON(ctx, "/api/media", url.Values{"item": {show}}, &it)
	epOK := code == 200 && len(it.Seasons) == 1 && it.Seasons[0].N == 1 && len(it.Seasons[0].Episodes) == 1 && it.Seasons[0].Episodes[0].Episode == 2
	r.check("I-211 I-212", "the show lists season 1 episode 2", epOK, fmt.Sprintf("%d %+v", code, it.Seasons))
	sameMedia := true
	_, want, _ := w.do(ctx, http.MethodGet, "/api/media", url.Values{"lib": {join(dir, "films")}}, nil, "")
	for i, c := range clients[1:] {
		_, got, _ := c.do(ctx, http.MethodGet, "/api/media", url.Values{"lib": {join(dir, "films")}}, nil, "")
		if !bytes.Equal(got, want) {
			sameMedia = false
			r.checkAt("I-212", "the same library answer on "+names[i+1], false, string(got), "", "", names[i+1])
		}
	}
	r.check("I-212", "every gateway lists the library with the same bytes", sameMedia, fmt.Sprintf("%d gateways", len(clients)))

	// (f) the ROMs: a platform, one card for the title's two dumps, the gamelist's text
	// after the pass, nothing played (I-278, I-279)
	var plat webui.MediaItem
	code, _ = w.getJSON(ctx, "/api/media", url.Values{"item": {snes}}, &plat)
	platOK := code == 200 && plat.Kind == "platform" && plat.Title == "Super Nintendo" && len(plat.Games) == 1 && plat.Games[0].Title == "Probe Game" && plat.Games[0].Variants == 1
	r.check("I-278 I-279", "the platform lists one card for the title's two dumps", platOK, fmt.Sprintf("%d %+v", code, plat))
	var g webui.MediaItem
	code, _ = w.getJSON(ctx, "/api/media", url.Values{"item": {game}}, &g)
	gameOK := code == 200 && g.Kind == "game" && g.Revision == "Rev 1" && strings.Contains(g.Plot, word) && len(g.Files) == 2 && g.Video == ""
	r.check("I-278 I-279", "the game carries its gamelist's text and its files, and nothing to play", gameOK, fmt.Sprintf("%d %+v", code, g))
	sameRoms := true
	_, want, _ = w.do(ctx, http.MethodGet, "/api/media", url.Values{"item": {snes}}, nil, "")
	for i, c := range clients[1:] {
		if _, got, _ := c.do(ctx, http.MethodGet, "/api/media", url.Values{"item": {snes}}, nil, ""); !bytes.Equal(got, want) {
			sameRoms = false
			r.checkAt("I-279", "the same platform answer on "+names[i+1], false, string(got), "", "", names[i+1])
		}
	}
	r.check("I-279", "every gateway lists the platform with the same bytes", sameRoms, fmt.Sprintf("%d gateways", len(clients)))
	return nil
}
