package soak

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/pins"
	"github.com/zombar/tailraid/internal/webui"
)

// --- 16 web ui ----------------------------------------------------------------------------

// webHTTP reaches a web UI when the environment offers no client of its own.
var webHTTP = &http.Client{Timeout: 60 * time.Second}

// webClient drives one node's browser UI over its JSON routes (docs/13 §3).
type webClient struct {
	base string
	c    *http.Client
}

func (w *webClient) do(ctx context.Context, method, route string, q url.Values, body io.Reader, contentType string) (int, []byte, error) {
	u := w.base + route
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, nil, err
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set(webui.HeaderUI, "1")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := w.c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// getJSON decodes a GET's body into out when the status is 2xx.
func (w *webClient) getJSON(ctx context.Context, route string, q url.Values, out any) (int, error) {
	code, b, err := w.do(ctx, http.MethodGet, route, q, nil, "")
	if err != nil {
		return 0, err
	}
	if code/100 == 2 && out != nil {
		err = json.Unmarshal(b, out)
	}
	return code, err
}

// postJSON sends in as JSON and decodes the reply into out when the status is 2xx.
func (w *webClient) postJSON(ctx context.Context, route string, in, out any) (int, []byte, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return 0, nil, err
	}
	code, b, err := w.do(ctx, http.MethodPost, route, nil, bytes.NewReader(body), "application/json")
	if err != nil {
		return 0, nil, err
	}
	if code/100 == 2 && out != nil && len(b) > 0 {
		err = json.Unmarshal(b, out)
	}
	return code, b, err
}

func (w *webClient) put(ctx context.Context, path string, data []byte) (int, error) {
	code, _, err := w.do(ctx, http.MethodPut, "/files"+encodeWebPath(path), nil, bytes.NewReader(data), "application/octet-stream")
	return code, err
}

func (w *webClient) get(ctx context.Context, path string, root uint64) (int, []byte, error) {
	q := url.Values{}
	if root != 0 {
		q.Set("in", fmt.Sprint(root))
	}
	code, b, err := w.do(ctx, http.MethodGet, "/files"+encodeWebPath(path), q, nil, "")
	return code, b, err
}

// encodeWebPath escapes each segment of a volume path for a URL.
func encodeWebPath(p string) string {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return "/" + strings.Join(parts, "/")
}

type webListing struct {
	Path    string        `json:"path"`
	Dir     webui.DirInfo `json:"dir"`
	Entries []webui.Entry `json:"entries"`
}

func (l webListing) entry(name string) *webui.Entry {
	for i := range l.Entries {
		if l.Entries[i].Name == name && l.Entries[i].DeletedAt == "" {
			return &l.Entries[i]
		}
	}
	return nil
}

func (l webListing) deleted(name string) *webui.Entry {
	for i := range l.Entries {
		if l.Entries[i].Name == name && l.Entries[i].DeletedAt != "" {
			return &l.Entries[i]
		}
	}
	return nil
}

type webBatch struct {
	Moved   []string        `json:"moved"`
	Deleted []string        `json:"deleted"`
	Failed  []webui.Failure `json:"failed"`
}

func (b webBatch) failed(path string) *webui.Failure {
	for i := range b.Failed {
		if b.Failed[i].Path == path {
			return &b.Failed[i]
		}
	}
	return nil
}

func (w *webClient) ls(ctx context.Context, path string) (webListing, int, error) {
	var l webListing
	code, err := w.getJSON(ctx, "/api/ls", url.Values{"path": {path}}, &l)
	return l, code, err
}

func (w *webClient) stat(ctx context.Context, path string) (webui.Entry, int, error) {
	var e webui.Entry
	code, err := w.getJSON(ctx, "/api/stat", url.Values{"path": {path}}, &e)
	return e, code, err
}

func (w *webClient) del(ctx context.Context, paths ...string) (webBatch, int, error) {
	var out webBatch
	code, _, err := w.postJSON(ctx, "/api/delete", map[string]any{"paths": paths}, &out)
	return out, code, err
}

func (w *webClient) trash(ctx context.Context) ([]admin.TrashEntry, error) {
	var out struct {
		Entries []admin.TrashEntry `json:"entries"`
	}
	code, err := w.getJSON(ctx, "/api/trash", nil, &out)
	if err == nil && code != 200 {
		err = fmt.Errorf("trash: %d", code)
	}
	return out.Entries, err
}

// --- fixtures ---------------------------------------------------------------------------

// id3v2 builds an ID3v2.3 tag with ISO-8859-1 text frames and a JPEG picture, the
// shape `/api/tags` reads (docs/13 §6).
func id3v2(fields map[string]string, pic []byte) []byte {
	ids := make([]string, 0, len(fields))
	for id := range fields {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var frames []byte
	frame := func(id string, body []byte) {
		frames = append(frames, id...)
		frames = binary.BigEndian.AppendUint32(frames, uint32(len(body)))
		frames = append(frames, 0, 0)
		frames = append(frames, body...)
	}
	for _, id := range ids {
		frame(id, append([]byte{0}, fields[id]...))
	}
	if pic != nil {
		body := make([]byte, 0, 14+len(pic))
		body = append(body, 0)
		body = append(body, "image/jpeg\x00"...)
		body = append(body, 3, 0)
		frame("APIC", append(body, pic...))
	}
	n := len(frames)
	hdr := make([]byte, 0, 10+len(frames))
	hdr = append(hdr, 'I', 'D', '3', 3, 0, 0, byte(n>>21)&0x7f, byte(n>>14)&0x7f, byte(n>>7)&0x7f, byte(n)&0x7f)
	return append(hdr, frames...)
}

func solidPNG(w, h int, c color.Color) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

func solidJPEG(w, h int, c color.Color) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	_ = jpeg.Encode(&b, img, &jpeg.Options{Quality: 85})
	return b.Bytes()
}

// --- the phase ---------------------------------------------------------------------------

// phaseWeb drives the browser UI of docs/13 over HTTP at the gateway's web_url, as a
// same-user device: listings, batches, the trash round trip against the mount, flags,
// thumbnails, tags, the integrity pages and a pinned folder read with its other holders
// down. Everything it changes is restored on the way out.
func phaseWeb(ctx context.Context, r *Run) error {
	gws := r.gateways()
	if len(gws) == 0 {
		r.skip("no gateway")
		return nil
	}
	gw := gws[0]
	base, err := r.Env.WebURL(ctx, gw)
	if err != nil {
		r.check("I-158", "the web UI's address known", false, err.Error())
		return nil
	}
	if base == "" {
		r.skip("no browser UI in this environment")
		return nil
	}
	hc := r.Env.WebClient()
	if hc == nil {
		hc = webHTTP
	}
	w := &webClient{base: strings.TrimSuffix(base, "/"), c: hc}
	v := r.volume(gw)
	caps := r.Env.Caps()
	u := r.undoer(ctx)
	defer u.run()
	cs := r.Profile.ChunkSize
	dir := join(r.Root, "web")
	// the corpus of this phase, through the mount: sizes straddling the chunk size
	m := model{
		join(dir, "a.bin"):        r.randomBytes(2*cs + 5),
		join(dir, "b.txt"):        []byte("version one\n"),
		join(dir, "sub", "c.bin"): r.randomBytes(cs - 1),
		join(dir, "sub", "d"):     {},
	}
	if err := r.writeModel(v, m); err != nil {
		r.checkAt("I-7", "the phase's files written", false, err.Error(), "", "", gw.Name)
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

	// listings and stats against the model
	l, code, err := w.ls(ctx, dir)
	if err != nil || code != 200 {
		r.check("I-72", "ls answers", false, fmt.Sprintf("%d %v", code, err))
		return nil
	}
	names := map[string]bool{}
	for i := range l.Entries {
		names[l.Entries[i].Name] = true
	}
	r.check("I-72", "the listing names the model's entries", names["a.bin"] && names["b.txt"] && names["sub"] && len(l.Entries) == 3, fmt.Sprintf("%v", names))
	st, code, _ := w.stat(ctx, join(dir, "a.bin"))
	r.check("I-72", "stat carries the size, inode and version", code == 200 && st.Size == uint64(len(m[join(dir, "a.bin")])) && st.Ino != 0 && st.Version > 0, fmt.Sprintf("%d %+v", code, st))

	// batches: rename, move and delete with a taken name and a protected entry inside
	r.webBatches(ctx, w, v, dir, caps)
	// the trash round trip against the mount
	r.webTrash(ctx, w, v, dir, m)
	// flags
	r.webFlags(ctx, w, v, dir, caps, u)
	// thumbnails and tags
	r.webMedia(ctx, w, v, dir)
	// the integrity pages of the corpus's big file
	r.webIntegrity(ctx, w, gw)
	// a pinned folder read with its other holders down
	r.webPins(ctx, w, v, gw, dir, caps, u)
	return nil
}

func (r *Run) webBatches(ctx context.Context, w *webClient, v Volume, dir string, caps Caps) {
	for _, p := range []string{join(dir, "mv", "f1"), join(dir, "mv", "f2"), join(dir, "to", "f2"), join(dir, "prot", "f3")} {
		if err := v.Mkdir(join(p, "..")); err != nil {
			r.check("I-159", "batch fixtures written", false, err.Error())
			return
		}
		if err := v.WriteAt(p, 0, []byte(p)); err != nil {
			r.check("I-159", "batch fixtures written", false, err.Error())
			return
		}
	}
	var out map[string]any
	code, _, err := w.postJSON(ctx, "/api/rename", map[string]string{"path": join(dir, "mv", "f1"), "name": "renamed"}, &out)
	r.check("I-159", "rename answers the new path", code == 200 && out["path"] == join(dir, "mv", "renamed"), fmt.Sprintf("%d %v %v", code, out, err))
	code, _, _ = w.postJSON(ctx, "/api/rename", map[string]string{"path": join(dir, "mv", "renamed"), "name": "f2"}, nil)
	r.check("I-159", "rename onto a taken name is 409", code == 409, fmt.Sprint(code))
	protected := false
	if caps.Mutate {
		code, _, _ = w.postJSON(ctx, "/api/flags", map[string]any{"path": join(dir, "prot"), "protected": true}, nil)
		protected = code == 200
		r.check("I-163", "a folder protected through the web", protected, fmt.Sprint(code))
	}
	paths := []string{join(dir, "mv", "renamed"), join(dir, "mv", "f2"), join(dir, "nope")}
	if protected {
		paths = append(paths, join(dir, "prot", "f3"))
	}
	var mv webBatch
	code, _, err = w.postJSON(ctx, "/api/move", map[string]any{"paths": paths, "to": join(dir, "to")}, &mv)
	ok := code == 200 && len(mv.Moved) == 1 && mv.Moved[0] == join(dir, "to", "renamed") && mv.failed(join(dir, "mv", "f2")) != nil && mv.failed(join(dir, "mv", "f2")).Status == 409 && mv.failed(join(dir, "nope")) != nil && mv.failed(join(dir, "nope")).Status == 404
	if protected {
		ok = ok && mv.failed(join(dir, "prot", "f3")) != nil && mv.failed(join(dir, "prot", "f3")).Status == 403
	}
	r.check("I-159", "move reports every item: one moved, a taken name 409, a missing one 404"+map[bool]string{true: ", a protected one 403", false: ""}[protected], ok, fmt.Sprintf("%d %+v %v", code, mv, err))
	if got, err := v.ReadAll(join(dir, "mv", "f2")); err != nil || string(got) != join(dir, "mv", "f2") {
		r.check("I-159", "the item that failed is untouched", false, fmt.Sprint(err))
	} else if got, err := v.ReadAll(join(dir, "to", "f2")); err != nil || string(got) != join(dir, "to", "f2") {
		r.check("I-159", "the item that failed is untouched", false, "the destination's own f2 changed")
	} else {
		r.check("I-159", "the item that failed is untouched", true, "f2 in place at both ends")
	}
	del, code, err := w.del(ctx, join(dir, "to", "renamed"), join(dir, "nope"))
	r.check("I-159", "delete reports every item: one deleted, a missing one 404", code == 200 && len(del.Deleted) == 1 && del.failed(join(dir, "nope")) != nil && del.failed(join(dir, "nope")).Status == 404, fmt.Sprintf("%d %+v %v", code, del, err))
	if protected {
		del, _, _ = w.del(ctx, join(dir, "prot", "f3"))
		r.check("I-154 I-163 L-206", "a delete under a protected folder is 403", len(del.Deleted) == 0 && del.failed(join(dir, "prot", "f3")) != nil && del.failed(join(dir, "prot", "f3")).Status == 403, fmt.Sprintf("%+v", del))
		code, _, _ = w.postJSON(ctx, "/api/rename", map[string]string{"path": join(dir, "prot"), "name": "prot2"}, nil)
		r.check("I-154 I-163 L-206", "renaming the protected folder is 403", code == 403, fmt.Sprint(code))
		err := v.Remove(join(dir, "prot", "f3"))
		r.check("I-154 L-206", "the mount's remove under protection is EPERM", errors.Is(err, os.ErrPermission), fmt.Sprint(err))
		code, err = w.put(ctx, join(dir, "prot", "new"), []byte("new"))
		r.check("I-154 I-163", "an upload into the protected folder succeeds", code == 201, fmt.Sprintf("%d %v", code, err))
		code, _, _ = w.postJSON(ctx, "/api/move", map[string]any{"paths": []string{join(dir, "to", "f2")}, "to": join(dir, "prot")}, &mv)
		r.check("I-154 I-163", "a move into the protected folder succeeds", code == 200 && len(mv.Moved) == 1, fmt.Sprintf("%d %+v", code, mv))
		code, _, _ = w.postJSON(ctx, "/api/flags", map[string]any{"path": join(dir, "prot"), "protected": false}, nil)
		r.check("I-163", "the folder unprotected again", code == 200, fmt.Sprint(code))
	} else {
		r.checkSkipped("I-154 I-163 L-206", "a delete under a protected folder is 403", "flags change the volume: needs --destructive")
	}
}

// attrCacheWindow is how long the driver gives a mount to show a change made through
// the browser to a name the mount itself deleted: a kernel NFS client trusts its
// directory attribute cache for up to acdirmax, 60 s by default (docs/07).
const attrCacheWindow = 90 * time.Second

// waitVisible returns once the mount answers a stat of p, or the window has passed.
func (r *Run) waitVisible(ctx context.Context, v Volume, p string) {
	deadline := time.Now().Add(attrCacheWindow)
	for {
		if _, err := v.Size(p); err == nil || time.Now().After(deadline) {
			return
		}
		if r.Env.Wait(ctx, 2*time.Second) != nil {
			return
		}
	}
}

func (r *Run) webTrash(ctx context.Context, w *webClient, v Volume, dir string, m model) {
	a := join(dir, "a.bin")
	before := m[a]
	st, _, _ := w.stat(ctx, a)
	del, code, err := w.del(ctx, a)
	if code != 200 || len(del.Deleted) != 1 {
		r.check("I-158", "a web delete lands", false, fmt.Sprintf("%d %+v %v", code, del, err))
		return
	}
	l, _, _ := w.ls(ctx, dir)
	gone := l.deleted("a.bin")
	r.check("I-155 I-158 L-199", "the deleted file is listed in place with its dates", gone != nil && gone.DeletedAt != "" && gone.Ino == st.Ino && l.entry("a.bin") == nil, fmt.Sprintf("%+v", gone))
	if gone == nil {
		return
	}
	code, got, err := w.get(ctx, "/", gone.Ino)
	r.check("I-155 I-158 L-199", "its bytes read through in= equal the bytes before the delete", code == 200 && bytes.Equal(got, before), fmt.Sprintf("%d %d bytes %v", code, len(got), err))
	// the share sees a delete
	entries, _ := v.List(dir)
	listed := false
	for _, n := range entries {
		listed = listed || n == "a.bin"
	}
	_, rerr := v.ReadAll(a)
	werr := v.WriteAt(a, 0, []byte("taken"))
	r.check("I-155", "the mount's listing omits it, a read fails and a new file takes the name", !listed && rerr != nil && werr == nil, fmt.Sprintf("listed %v, read %v, write %v", listed, rerr, werr))
	// a restore onto the taken name is refused, then done under a new name
	code, body, _ := w.postJSON(ctx, "/api/restore", map[string]any{"ino": gone.Ino}, nil)
	var out map[string]any
	code2, _, _ := w.postJSON(ctx, "/api/restore", map[string]any{"ino": gone.Ino, "name": "a (restored).bin"}, &out)
	restored, rerr := v.ReadAll(join(dir, "a (restored).bin"))
	r.check("I-152 I-158 L-200", "a restore onto a taken name is 409 {taken}, then lands under a new name with the same bytes", code == 409 && strings.Contains(string(body), `"taken":true`) && code2 == 200 && rerr == nil && bytes.Equal(restored, before), fmt.Sprintf("%d %d %v", code, code2, rerr))
	// rm -rf through the mount, restored with one call
	tree := join(dir, "tree")
	tm := model{join(tree, "x"): r.randomBytes(100), join(tree, "y", "z"): r.randomBytes(200)}
	if err := r.writeModel(v, tm); err == nil {
		for _, p := range []string{join(tree, "x"), join(tree, "y", "z"), join(tree, "y"), tree} {
			if err := v.Remove(p); err != nil {
				r.check("I-152 L-200", "rm -rf through the mount", false, p+": "+err.Error())
				return
			}
		}
		l, _, _ = w.ls(ctx, dir)
		root := l.deleted("tree")
		roots := 0
		for i := range l.Entries {
			if l.Entries[i].DeletedAt != "" {
				roots++
			}
		}
		code = 0
		if root != nil {
			code, _, _ = w.postJSON(ctx, "/api/restore", map[string]any{"ino": root.Ino}, nil)
		}
		// the mount deleted these names itself, so a kernel NFS client holds them as
		// absent until its attribute cache expires (docs/07); the browser's restore is
		// visible there within that window
		r.waitVisible(ctx, v, join(tree, "x"))
		bad, first := r.verifyModel(v, tm)
		r.check("I-152 L-200", "a tree removed piece by piece is one deleted entry and restores with one call", root != nil && roots == 1 && code == 200 && bad == 0, fmt.Sprintf("roots %d, restore %d, %s", roots, code, first))
	}
	// a dirty file deleted at once and restored with every acknowledged byte
	dirty := join(dir, "dirty.bin")
	data := r.randomBytes(3 * r.Profile.ChunkSize / 2)
	if err := v.WriteAt(dirty, 0, data); err == nil {
		del, _, _ = w.del(ctx, dirty)
		l, _, _ = w.ls(ctx, dir)
		if d := l.deleted("dirty.bin"); d != nil && len(del.Deleted) == 1 {
			code, _, _ = w.postJSON(ctx, "/api/restore", map[string]any{"ino": d.Ino}, nil)
			got, err := v.ReadAll(dirty)
			r.check("L-203", "a file deleted while dirty restores with every acknowledged byte", code == 200 && err == nil && bytes.Equal(got, data), fmt.Sprintf("%d %v %d bytes", code, err, len(got)))
		} else {
			r.check("L-203", "a file deleted while dirty restores with every acknowledged byte", false, fmt.Sprintf("%+v", del))
		}
	}
	// an overwrite through the web and through the mount keeps the previous version
	b := join(dir, "b.txt")
	code, _ = w.put(ctx, b, []byte("version two\n"))
	l, _, _ = w.ls(ctx, dir)
	prev := l.deleted("b.txt")
	var old []byte
	if prev != nil {
		_, old, _ = w.get(ctx, "/", prev.Ino)
	}
	r.check("I-158 L-205", "a web overwrite keeps the previous version in the trash", code == 204 && prev != nil && string(old) == "version one\n", fmt.Sprintf("%d %+v %q", code, prev, old))
	if err := v.WriteAt(join(dir, "b.tmp"), 0, []byte("version three\n")); err == nil {
		if err := v.Rename(join(dir, "b.tmp"), b); err == nil {
			l, _, _ = w.ls(ctx, dir)
			versions := map[string]bool{}
			for i := range l.Entries {
				if l.Entries[i].Name == "b.txt" && l.Entries[i].DeletedAt != "" {
					_, body, _ := w.get(ctx, "/", l.Entries[i].Ino)
					versions[string(body)] = true
				}
			}
			live, _ := v.ReadAll(b)
			r.check("L-205", "a save by rename on the mount keeps the previous version in the trash", len(versions) == 2 && versions["version one\n"] && versions["version two\n"] && string(live) == "version three\n", fmt.Sprintf("%d versions in the trash, live %q", len(versions), live))
		}
	}
	// ephemeral names never enter the trash
	for _, p := range []string{join(dir, ".DS_Store"), join(dir, ".z.tailraid-upload")} {
		if err := v.WriteAt(p, 0, []byte("x")); err != nil {
			continue
		}
		_ = v.Remove(p)
	}
	tr, err := w.trash(ctx)
	eph := false
	for _, e := range tr {
		eph = eph || e.Name == ".DS_Store" || strings.HasSuffix(e.Name, meta.UploadSuffix)
	}
	r.check("I-157 L-208", ".DS_Store and an upload temp deleted on the mount are absent from the trash", err == nil && !eph, fmt.Sprintf("%d entries", len(tr)))
	// a purge, then the admin purge and GC's reclaim after grace
	if prev != nil {
		code, _, _ = w.postJSON(ctx, "/api/purge", map[string]any{"ino": prev.Ino}, nil)
		l, _, _ = w.ls(ctx, dir)
		still := false
		for i := range l.Entries {
			still = still || l.Entries[i].Ino == prev.Ino
		}
		r.check("I-153 L-201", "a purge removes the entry", code == 204 && !still, fmt.Sprint(code))
	}
	purged, perr := r.Env.PurgeTrash(ctx)
	r.check("I-153 L-201", "the admin purge empties the trash", perr == nil && purged > 0, fmt.Sprintf("%d purged, %v", purged, perr))
	if err := r.Env.Wait(ctx, r.Profile.Grace+time.Second); err != nil {
		return
	}
	// as phase 8 judges it: the purged chunks were tombstoned and their deletes went out.
	// Stored bytes are no measure on a live cluster, where a migration a previous phase
	// started may be adding shards at the same time (the three-VM rounds).
	res, err := r.gcNow(ctx)
	r.check("I-13 L-201", "gc reclaims the purged files' chunks after grace", err == nil && res.Victims > 0 && res.Deleted > 0, fmt.Sprintf("%+v, %v", res, err))
}

func (r *Run) webFlags(ctx context.Context, w *webClient, v Volume, dir string, caps Caps, u *undo) {
	if !caps.Mutate {
		r.checkSkipped("I-163", "nsfw is effective on a nested file's stat", "flags change the volume: needs --destructive")
		return
	}
	n := join(dir, "n")
	if err := v.WriteAt(join(n, "deep", "f"), 0, []byte("f")); err != nil {
		if err := v.Mkdir(join(n, "deep")); err != nil || v.WriteAt(join(n, "deep", "f"), 0, []byte("f")) != nil {
			r.check("I-163", "nsfw fixture written", false, fmt.Sprint(err))
			return
		}
	}
	code, _, _ := w.postJSON(ctx, "/api/flags", map[string]any{"path": n, "nsfw": true}, nil)
	u.add("clear nsfw", func(c context.Context) error {
		_, _, err := w.postJSON(c, "/api/flags", map[string]any{"path": n, "nsfw": false}, nil)
		return err
	})
	st, code2, _ := w.stat(ctx, join(n, "deep", "f"))
	l, _, _ := w.ls(ctx, join(n, "deep"))
	r.check("I-163", "nsfw is effective on a nested file's stat and its folder's listing", code == 200 && code2 == 200 && st.NSFW && l.Dir.NSFW, fmt.Sprintf("%d %d %+v %+v", code, code2, st.NSFW, l.Dir))
	code, _, _ = w.postJSON(ctx, "/api/flags", map[string]any{"path": join(n, "deep", "f"), "nsfw": true}, nil)
	r.check("I-163", "flags on a file are 400", code == 400, fmt.Sprint(code))
}

func (r *Run) webMedia(ctx context.Context, w *webClient, v Volume, dir string) {
	clip := join(dir, "clip.mp4")
	if err := v.WriteAt(clip, 0, r.randomBytes(1000)); err != nil {
		r.check("I-160", "media fixtures written", false, err.Error())
		return
	}
	st, _, _ := w.stat(ctx, clip)
	frame := solidJPEG(320, 180, color.NRGBA{R: 30, G: 120, B: 200, A: 255})
	t0 := time.Now()
	code, _, err := w.do(ctx, http.MethodPut, "/api/thumb", url.Values{"path": {clip}, "version": {fmt.Sprint(st.Version)}}, bytes.NewReader(frame), "image/jpeg")
	code2, back, _ := w.do(ctx, http.MethodGet, "/api/thumb", url.Values{"path": {clip}}, nil, "")
	r.addThumb(time.Since(t0))
	r.check("I-160", "a JPEG PUT for a video reads back byte-exact", code == 201 && code2 == 200 && bytes.Equal(back, frame), fmt.Sprintf("%d %d %d bytes %v", code, code2, len(back), err))
	code, _, _ = w.do(ctx, http.MethodPut, "/api/thumb", url.Values{"path": {clip}, "version": {fmt.Sprint(st.Version + 1)}}, bytes.NewReader(frame), "image/jpeg")
	r.check("I-160", "a PUT for a stale version is 409", code == 409, fmt.Sprint(code))
	pic := join(dir, "pic.png")
	if err := v.WriteAt(pic, 0, solidPNG(640, 360, color.NRGBA{R: 200, G: 40, B: 40, A: 255})); err == nil {
		t0 = time.Now()
		code, body, _ := w.do(ctx, http.MethodGet, "/api/thumb", url.Values{"path": {pic}}, nil, "")
		r.addThumb(time.Since(t0))
		img, derr := jpeg.Decode(bytes.NewReader(body))
		r.check("I-160", "an image thumbnail is made by the node, at most 320 px wide", code == 200 && derr == nil && img != nil && img.Bounds().Dx() <= webui.ThumbWidth, fmt.Sprintf("%d %v", code, derr))
		l, _, _ := w.ls(ctx, dir)
		r.check("I-160", "the listing says the image has a thumbnail", l.entry("pic.png") != nil && l.entry("pic.png").Thumb, "")
	}
	code, _, _ = w.postJSON(ctx, "/api/mkdir", map[string]string{"path": "/" + meta.ReservedDir + "/mine"}, nil)
	code2, _ = w.put(ctx, "/"+meta.ReservedDir+"/thumbs/mine.jpg", frame)
	r.check("I-160", "user operations on /.tailraid are 400", code == 400 && code2 == 400, fmt.Sprintf("%d %d", code, code2))
	// tags and the folder's cover
	music := join(dir, "music")
	cover := solidJPEG(64, 64, color.NRGBA{R: 40, G: 200, B: 40, A: 255})
	track := append(id3v2(map[string]string{"TIT2": "Mascara", "TPE1": "Deftones", "TALB": "Around the Fur", "TRCK": "3/10", "TYER": "1997"}, cover), r.randomBytes(2000)...)
	plain := append(id3v2(map[string]string{"TIT2": "Intro"}, nil), r.randomBytes(500)...)
	if err := v.Mkdir(music); err != nil || v.WriteAt(join(music, "01 intro.mp3"), 0, plain) != nil || v.WriteAt(join(music, "03 mascara.mp3"), 0, track) != nil {
		r.check("I-164", "audio fixtures written", false, fmt.Sprint(err))
		return
	}
	var tg webui.Tags
	code, err = w.getJSON(ctx, "/api/tags", url.Values{"path": {join(music, "03 mascara.mp3")}}, &tg)
	r.check("I-164", "/api/tags returns the tag's fields", code == 200 && err == nil && tg.Title == "Mascara" && tg.Artist == "Deftones" && tg.Album == "Around the Fur" && tg.Track == 3 && tg.Tracks == 10 && tg.Year == 1997 && tg.HasCover, fmt.Sprintf("%d %+v %v", code, tg, err))
	code, body, _ := w.do(ctx, http.MethodGet, "/api/thumb", url.Values{"path": {join(music, "03 mascara.mp3")}}, nil, "")
	img, derr := jpeg.Decode(bytes.NewReader(body))
	r.check("I-164 I-165", "/api/thumb serves the embedded picture", code == 200 && derr == nil && img != nil, fmt.Sprintf("%d %v", code, derr))
	var none webui.Tags
	code, _ = w.getJSON(ctx, "/api/tags", url.Values{"path": {join(dir, "b.txt")}}, &none)
	code2, _ = w.getJSON(ctx, "/api/tags", url.Values{"path": {clip}}, &none)
	r.check("I-164", "tags of a non-audio file are 400", code == 400 && code2 == 400, fmt.Sprintf("%d %d", code, code2))
	l, _, _ := w.ls(ctx, music)
	root, _, _ := w.ls(ctx, dir)
	r.check("I-165", "the folder's cover is its first audio entry by name with a picture", l.Dir.Cover == "03 mascara.mp3" && root.entry("music") != nil && root.entry("music").Cover == "03 mascara.mp3", fmt.Sprintf("%q %+v", l.Dir.Cover, root.entry("music")))
}

func (r *Run) webIntegrity(ctx context.Context, w *webClient, gw NodeRef) {
	m := r.corpusModel()
	var path string
	for p := range m {
		if strings.HasSuffix(p, "/big.bin") {
			path = p
		}
	}
	if path == "" {
		r.checkSkipped("I-161", "the integrity pages agree with the chunk reports over RPC", "no corpus big file")
		return
	}
	want, err := r.Env.Chunks(ctx, gw, path)
	if err != nil {
		r.check("I-161", "chunk reports over RPC", false, err.Error())
		return
	}
	type page struct {
		Total  int `json:"total"`
		Chunks []struct {
			Hash  string          `json:"hash"`
			Slots json.RawMessage `json:"slots"`
		} `json:"chunks"`
	}
	var hashes []string
	nullSlots := 0
	for off := 0; ; {
		var pg page
		code, err := w.getJSON(ctx, "/api/chunks", url.Values{"path": {path}, "offset": {fmt.Sprint(off)}, "limit": {"2"}}, &pg)
		if err != nil || code != 200 {
			r.check("I-161", "the integrity pages answer", false, fmt.Sprintf("%d %v", code, err))
			return
		}
		for _, c := range pg.Chunks {
			hashes = append(hashes, c.Hash)
			if len(c.Slots) == 0 || string(c.Slots) == "null" {
				nullSlots++
			}
		}
		off += len(pg.Chunks)
		if len(pg.Chunks) == 0 || off >= pg.Total {
			break
		}
	}
	same := len(hashes) == len(want)
	for i := range want {
		same = same && i < len(hashes) && hashes[i] == want[i].Hash
	}
	r.check("I-161", "every integrity page carries slots as an array and the hashes equal the chunk reports over RPC", same && nullSlots == 0 && len(hashes) > 0, fmt.Sprintf("%d of %d chunks, %d null slots", len(hashes), len(want), nullSlots))
}

func (r *Run) webPins(ctx context.Context, w *webClient, v Volume, gw NodeRef, dir string, caps Caps, u *undo) {
	pinDir := join(dir, "pinned")
	data := r.randomBytes(3*r.Profile.ChunkSize + 11)
	if err := v.Mkdir(pinDir); err != nil || v.WriteAt(join(pinDir, "f.bin"), 0, data) != nil {
		r.check("I-162", "pin fixture written", false, fmt.Sprint(err))
		return
	}
	if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
		return
	}
	var out map[string]any
	code, body, err := w.postJSON(ctx, "/api/pin", map[string]any{"path": pinDir, "pinned": true}, &out)
	if code == 503 {
		r.checkSkipped("I-162", "a pinned folder's chunks arrive in the cache", "pins are not wired on this node")
		return
	}
	if code != 200 {
		r.check("I-162", "pin accepted", false, fmt.Sprintf("%d %s %v", code, body, err))
		return
	}
	u.add("unpin", func(c context.Context) error {
		_, _, err := w.postJSON(c, "/api/pin", map[string]any{"path": pinDir, "pinned": false}, nil)
		return err
	})
	var rep pins.Report
	arrived := false
	for range 100 {
		if _, err := w.getJSON(ctx, "/api/pins", nil, &rep); err == nil {
			for _, p := range rep.Pins {
				if p.Path == pinDir && p.Bytes > 0 && p.CachedBytes >= p.Bytes {
					arrived = true
				}
			}
		}
		if arrived {
			break
		}
		if err := r.Env.Wait(ctx, 200*time.Millisecond); err != nil {
			return
		}
	}
	st, _ := r.Env.Status(ctx, gw)
	r.check("I-162", "a pinned folder's chunks arrive in the cache and status reports them", arrived && st.Cache != nil && st.Cache.PinnedBytes >= int64(len(data)), fmt.Sprintf("%+v", rep))
	l, _, _ := w.ls(ctx, pinDir)
	r.check("I-162", "the folder's listing says it is kept here", l.Dir.Pinned, fmt.Sprintf("%+v", l.Dir))
	if !caps.Kill {
		r.checkSkipped("I-162", "the pinned folder reads with its other holders down", "needs Kill")
	} else if arrived {
		r.webReadOffline(ctx, w, gw, join(pinDir, "f.bin"), data, u)
	}
	code, _, _ = w.postJSON(ctx, "/api/pin", map[string]any{"path": pinDir, "pinned": false}, nil)
	u.done("unpin")
	rep2, _ := r.Env.Status(ctx, gw)
	released := code == 200 && (rep2.Cache == nil || st.Cache == nil || rep2.Cache.PinnedBytes < st.Cache.PinnedBytes)
	r.check("I-162", "an unpin releases the folder", released, fmt.Sprintf("%d", code))
}

// webReadOffline stops the pinned file's other holders, enough that a cold read would
// fail, never below a majority of voters, and reads the file through the web.
func (r *Run) webReadOffline(ctx context.Context, w *webClient, gw NodeRef, path string, want []byte, u *undo) {
	reps, err := r.Env.Chunks(ctx, gw, path)
	rep, _, serr := r.anyStatus(ctx)
	if err != nil || serr != nil || len(reps) == 0 {
		r.check("I-162", "the pinned folder reads with its other holders down", false, fmt.Sprintf("%v %v", err, serr))
		return
	}
	hostOf := map[int]meta.NodeIdx{}
	voter := map[meta.NodeIdx]bool{}
	for i := range rep.Nodes {
		n := &rep.Nodes[i]
		switch n.Kind {
		case "disk":
			hostOf[int(n.Idx)] = n.Host
		case "device":
			hostOf[int(n.Idx)] = n.Idx
			voter[n.Idx] = n.Voter && n.Live
		}
	}
	holders := map[meta.NodeIdx]bool{}
	for i := range reps {
		for _, s := range reps[i].Slots {
			if h, ok := hostOf[s.Node]; ok && h != gw.Idx && s.Node != 0 {
				holders[h] = true
			}
		}
	}
	k := int(reps[0].K)
	liveVoters := 0
	for _, isVoter := range voter {
		if isVoter {
			liveVoters++
		}
	}
	majority := len(voter)/2 + 1
	var killed []NodeRef
	// stop other holders until fewer than k live holders remain for every chunk, while a
	// majority of voters stays up
	remaining := len(holders) + 1 // the gateway may hold a slot itself
	for h := range holders {
		if remaining-1 < k && len(killed) > 0 {
			break
		}
		if voter[h] && liveVoters <= majority {
			continue
		}
		n, ok := r.nodeByIdx(int(h))
		if !ok {
			continue
		}
		if err := r.Env.Kill(ctx, n); err != nil {
			continue
		}
		killed = append(killed, n)
		if voter[h] {
			liveVoters--
		}
		remaining--
		r.event("kill", n.Name+" (a holder of the pinned folder)")
	}
	u.add("start the holders", func(c context.Context) error {
		for _, n := range killed {
			if err := r.Env.Start(c, n); err != nil {
				return err
			}
		}
		return nil
	})
	if len(killed) == 0 {
		r.checkSkipped("I-162", "the pinned folder reads with its other holders down", "no holder could be stopped without losing the majority")
		return
	}
	code, got, err := w.get(ctx, path, 0)
	r.check("I-162", fmt.Sprintf("the pinned folder reads through the web with %d holders down", len(killed)), code == 200 && err == nil && bytes.Equal(got, want), fmt.Sprintf("%d %v %d bytes", code, err, len(got)))
	for _, n := range killed {
		if err := r.Env.Start(ctx, n); err != nil {
			r.event("note", "restart "+n.Name+": "+err.Error())
		}
	}
	u.done("start the holders")
	if err := r.Env.Wait(ctx, r.Profile.RestartSettle); err != nil {
		return
	}
}

// addThumb records one thumbnail round trip for the totals.
func (r *Run) addThumb(d time.Duration) {
	r.mu.Lock()
	r.thumbs++
	r.thumbMax = max(r.thumbMax, d)
	r.mu.Unlock()
}
