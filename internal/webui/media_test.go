package webui_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/tailscale"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/webui"
)

// --- fixtures, built in the test: no binaries checked in -------------------------------

func solid(w, h int, c color.Color) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	return img
}

func pngBytes(w, h int, c color.Color) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, solid(w, h, c)); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func jpegBytes(w, h int, c color.Color) []byte {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, solid(w, h, c), &jpeg.Options{Quality: 90}); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// hugePNG is a PNG whose header claims w×h pixels and whose data never comes: enough
// for DecodeConfig, and what the pixel bound must refuse before decoding.
func hugePNG(w, h int) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8], ihdr[9] = 8, 6 // depth, RGBA
	chunk := append([]byte("IHDR"), ihdr...)
	b := []byte("\x89PNG\r\n\x1a\n")
	b = binary.BigEndian.AppendUint32(b, 13)
	b = append(b, chunk...)
	b = binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(chunk))
	return b
}

func id3Frame(id string, body []byte) []byte {
	b := []byte(id)
	b = binary.BigEndian.AppendUint32(b, uint32(len(body)))
	b = append(b, 0, 0)
	return append(b, body...)
}

// id3v2 builds an ID3v2.3 tag: text frames (ISO-8859-1) and an APIC picture.
func id3v2(fields map[string]string, pic []byte) []byte {
	ids := make([]string, 0, len(fields))
	for id := range fields {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var frames []byte
	for _, id := range ids {
		frames = append(frames, id3Frame(id, append([]byte{0}, fields[id]...))...)
	}
	if pic != nil {
		body := []byte{0}
		body = append(body, "image/jpeg\x00"...)
		body = append(body, 3, 0)
		body = append(body, pic...)
		frames = append(frames, id3Frame("APIC", body)...)
	}
	return append(id3Header(len(frames)), frames...)
}

func id3Header(n int) []byte {
	return []byte{'I', 'D', '3', 3, 0, 0, byte(n>>21) & 0x7f, byte(n>>14) & 0x7f, byte(n>>7) & 0x7f, byte(n) & 0x7f}
}

// id3v1 is the 128-byte tail tag, ID3v1.1 with a track number.
func id3v1(title, artist, album, year string, track byte) []byte {
	pad := func(s string, n int) []byte { return append([]byte(s), make([]byte, n-len(s))...) }
	b := []byte("TAG")
	b = append(b, pad(title, 30)...)
	b = append(b, pad(artist, 30)...)
	b = append(b, pad(album, 30)...)
	b = append(b, pad(year, 4)...)
	comment := make([]byte, 30)
	comment[29] = track
	b = append(b, comment...)
	return append(b, 17) // genre 17: Rock
}

// flacFile is a FLAC stream's metadata: STREAMINFO, a Vorbis comment block and an
// optional picture block; no audio frames follow.
func flacFile(comments []string, pic []byte) []byte {
	b := []byte("fLaC")
	b = append(b, 0, 0, 0, 34)
	b = append(b, make([]byte, 34)...)
	vendor := "tailraid test"
	vc := binary.LittleEndian.AppendUint32(nil, uint32(len(vendor)))
	vc = append(vc, vendor...)
	vc = binary.LittleEndian.AppendUint32(vc, uint32(len(comments)))
	for _, c := range comments {
		vc = binary.LittleEndian.AppendUint32(vc, uint32(len(c)))
		vc = append(vc, c...)
	}
	typ := byte(4)
	if pic == nil {
		typ |= 0x80
	}
	b = append(b, typ, byte(len(vc)>>16), byte(len(vc)>>8), byte(len(vc)))
	b = append(b, vc...)
	if pic != nil {
		p := binary.BigEndian.AppendUint32(nil, 3)
		p = binary.BigEndian.AppendUint32(p, uint32(len("image/jpeg")))
		p = append(p, "image/jpeg"...)
		p = binary.BigEndian.AppendUint32(p, 0)
		for range 4 {
			p = binary.BigEndian.AppendUint32(p, 0)
		}
		p = binary.BigEndian.AppendUint32(p, uint32(len(pic)))
		p = append(p, pic...)
		b = append(b, 0x86, byte(len(p)>>16), byte(len(p)>>8), byte(len(p)))
		b = append(b, p...)
	}
	return b
}

func (u *ui) thumb(query string) (int, http.Header, []byte) {
	u.t.Helper()
	return u.get("/api/thumb?" + query)
}

func decodeJPEG(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}
	return img
}

// hue names the dominant channel of the image's centre pixel.
func hue(img image.Image) string {
	b := img.Bounds()
	r, g, bl, _ := img.At(b.Min.X+b.Dx()/2, b.Min.Y+b.Dy()/2).RGBA()
	switch {
	case r > g && r > bl:
		return "red"
	case g > r && g > bl:
		return "green"
	case bl > r && bl > g:
		return "blue"
	}
	return "grey"
}

var (
	red   = color.NRGBA{R: 220, G: 20, B: 20, A: 255}
	green = color.NRGBA{R: 20, G: 220, B: 20, A: 255}
	blue  = color.NRGBA{R: 20, G: 20, B: 220, A: 255}
)

// The node makes an image's thumbnail on the first GET, at most 320 px wide, stores it
// under /.tailraid/thumbs/<ino>-<gen>-<version>.jpg and serves the stored one after;
// /.tailraid is absent from the listing and refused as a target; a picture over the
// pixel bound is 415; a video without a stored thumbnail, a text file and a folder
// have none.
//
// Invariant: I-160
func TestWebUI_Thumb_Image_Generated_Cached_Hidden(t *testing.T) {
	u := newUI(t)
	u.write("/pic.png", 0, pngBytes(640, 360, red))
	u.write("/small.jpg", 0, jpegBytes(200, 100, blue))
	u.write("/huge.png", 0, hugePNG(9000, 9000))
	u.write("/clip.mp4", 0, []byte("not really a video"))
	u.write("/note.txt", 0, []byte("text"))
	u.write("/d/x", 0, []byte("x"))
	u.flush()
	code, hdr, body := u.thumb("path=/pic.png")
	if code != 200 || hdr.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumb: %d %s", code, hdr.Get("Content-Type"))
	}
	img := decodeJPEG(t, body)
	if img.Bounds().Dx() != webui.ThumbWidth || img.Bounds().Dy() != 180 || hue(img) != "red" {
		t.Fatalf("thumb %v %s", img.Bounds(), hue(img))
	}
	in := u.statOf("/pic.png")
	key := fmt.Sprintf("%d-%d-%d.jpg", in.Ino, u.f.Inode(meta.Ino(in.Ino)).Gen, in.Version)
	if !u.exists(webui.ThumbDir+"/"+key) || hdr.Get("ETag") != `"`+key+`"` {
		t.Fatalf("stored thumbnail %s missing (etag %q)", key, hdr.Get("ETag"))
	}
	if !in.Thumb {
		t.Fatal("the entry does not say it has a thumbnail")
	}
	// the stored one is served: replace it with other bytes and read it back
	must(t, u.fs.Remove(webui.ThumbDir+"/"+key))
	u.write(webui.ThumbDir+"/"+key, 0, jpegBytes(10, 10, green))
	if _, _, body = u.thumb("path=/pic.png"); hue(decodeJPEG(t, body)) != "green" {
		t.Fatal("the stored thumbnail was not the one served")
	}
	if _, _, body = u.thumb("path=/small.jpg"); decodeJPEG(t, body).Bounds().Dx() != 200 {
		t.Fatal("a small image was scaled up")
	}
	for query, want := range map[string]int{"path=/huge.png": 415, "path=/clip.mp4": 404, "path=/note.txt": 404, "path=/d": 404, "path=/nope": 404} {
		if code, _, _ := u.thumb(query); code != want {
			t.Fatalf("thumb %s: %d, want %d", query, code, want)
		}
	}
	if l := u.lsOf("path=/"); l.entry(".tailraid") != nil || !l.entry("pic.png").Thumb || l.entry("note.txt").Thumb {
		t.Fatalf("listing: %+v", l.Entries)
	}
	if code, _ := u.put("/.tailraid/thumbs/mine.jpg", []byte("x")); code != 400 {
		t.Fatalf("put under /.tailraid: %d", code)
	}
	if code, _ := u.mkdir("/.tailraid/more", webui.HeaderUI, "1"); code != 400 {
		t.Fatalf("mkdir under /.tailraid: %d", code)
	}
	if code := u.post("/api/rename", `{"path":"/.tailraid/thumbs","name":"t"}`, nil); code != 400 {
		t.Fatalf("rename under /.tailraid: %d", code)
	}
	// a thumbnail inside a deleted root is served through in=
	root := u.inoOf("/d")
	u.write("/d/p.png", 0, pngBytes(40, 20, blue))
	u.flush()
	u.del("/d")
	if code, _, body := u.thumb(fmt.Sprintf("path=/p.png&in=%d", root)); code != 200 || hue(decodeJPEG(t, body)) != "blue" {
		t.Fatalf("thumb inside the trash: %d", code)
	}
}

// PUT /api/thumb: bodies of {0, 1, max−1, max, max+1} bytes, the JPEG magic, the
// file's current version; the stored bytes come back as they went in.
//
// Invariant: I-160, I-116
func TestWebUI_Thumb_Put_Boundaries(t *testing.T) {
	u := newUI(t)
	u.write("/clip.mp4", 0, []byte("video"))
	u.flush()
	in := u.statOf("/clip.mp4")
	putThumb := func(query string, body []byte) int {
		resp := u.do(http.MethodPut, "/api/thumb?"+query, bytes.NewReader(body), webui.HeaderUI, "1")
		resp.Body.Close()
		return resp.StatusCode
	}
	jpg := func(n int) []byte {
		b := make([]byte, n)
		copy(b, "\xFF\xD8\xFF")
		return b
	}
	q := fmt.Sprintf("path=/clip.mp4&version=%d", in.Version)
	for _, tc := range []struct {
		n    int
		want int
	}{{0, 415}, {1, 415}, {webui.ThumbPutMax - 1, 201}, {webui.ThumbPutMax, 201}, {webui.ThumbPutMax + 1, 400}} {
		if code := putThumb(q, jpg(tc.n)); code != tc.want {
			t.Fatalf("%d bytes: %d, want %d", tc.n, code, tc.want)
		}
	}
	if code := putThumb(q, []byte("\x89PNG\r\n\x1a\n")); code != 415 {
		t.Fatalf("a PNG body: %d", code)
	}
	if code := putThumb(fmt.Sprintf("path=/clip.mp4&version=%d", in.Version+1), jpg(100)); code != 409 {
		t.Fatalf("a stale version: %d", code)
	}
	for query, want := range map[string]int{"path=/clip.mp4": 400, "path=/nope&version=1": 404, "path=/&version=1": 400, "path=/.tailraid/x&version=1": 400} {
		if code := putThumb(query, jpg(100)); code != want {
			t.Fatalf("%s: %d, want %d", query, code, want)
		}
	}
	frame := jpegBytes(300, 169, green)
	if code := putThumb(q, frame); code != 201 {
		t.Fatalf("a real frame: %d", code)
	}
	code, _, body := u.thumb("path=/clip.mp4")
	if code != 200 || !bytes.Equal(body, frame) {
		t.Fatalf("the stored frame came back changed: %d, %d bytes", code, len(body))
	}
	if !u.lsOf("path=/").entry("clip.mp4").Thumb {
		t.Fatal("the entry does not say it has a thumbnail")
	}
}

// A changed file gets a new key and the sweep removes the thumbnail whose (ino, gen,
// version) no longer names a file; the previous version of an overwritten file and a
// deleted file keep theirs while they sit in the trash and lose them when purged; a
// foreign file under /.tailraid/thumbs is left alone.
//
// Invariant: I-160
func TestWebUI_Thumb_SweptWhenFileChanges(t *testing.T) {
	u := newUI(t)
	u.write("/pic.png", 0, pngBytes(32, 16, red))
	u.write("/gone.png", 0, pngBytes(32, 16, blue))
	u.flush()
	key := func(path string) string {
		in := u.statOf(path)
		return fmt.Sprintf("%d-%d-%d.jpg", in.Ino, u.f.Inode(meta.Ino(in.Ino)).Gen, in.Version)
	}
	sweep := func() int {
		t.Helper()
		n, err := webui.SweepThumbs(context.Background(), u.fs, u.f.FSM)
		must(t, err)
		return n
	}
	for _, p := range []string{"/pic.png", "/gone.png"} {
		if code, _, _ := u.thumb("path=" + p); code != 200 {
			t.Fatalf("thumb %s: %d", p, code)
		}
	}
	v1, goneKey := key("/pic.png"), key("/gone.png")
	// a write in place: the same inode at a new version, the old key an orphan
	u.write("/pic.png", 0, pngBytes(32, 16, blue))
	u.flush()
	v2 := key("/pic.png")
	if v2 == v1 || sweep() != 1 || u.exists(webui.ThumbDir+"/"+v1) {
		t.Fatalf("after the in-place change: old key %v, %s vs %s", u.exists(webui.ThumbDir+"/"+v1), v1, v2)
	}
	if code, _, body := u.thumb("path=/pic.png"); code != 200 || hue(decodeJPEG(t, body)) != "blue" {
		t.Fatalf("thumb after the change: %d", code)
	}
	// an overwrite: the previous version goes to the trash with its thumbnail
	if code, _ := u.put("/pic.png", pngBytes(32, 16, green)); code != 204 {
		t.Fatalf("overwrite: %d", code)
	}
	u.flush()
	if code, _, body := u.thumb("path=/pic.png"); code != 200 || hue(decodeJPEG(t, body)) != "green" {
		t.Fatalf("thumb of the new version: %d", code)
	}
	v3 := key("/pic.png")
	u.write(webui.ThumbDir+"/foreign.jpg", 0, []byte("x"))
	u.del("/gone.png")
	u.flush()
	if n := sweep(); n != 0 || !u.exists(webui.ThumbDir+"/"+v2) || !u.exists(webui.ThumbDir+"/"+goneKey) || !u.exists(webui.ThumbDir+"/foreign.jpg") {
		t.Fatalf("sweep with the trash full removed %d: previous version's %v, deleted file's %v, foreign %v", n, u.exists(webui.ThumbDir+"/"+v2), u.exists(webui.ThumbDir+"/"+goneKey), u.exists(webui.ThumbDir+"/foreign.jpg"))
	}
	must(t, u.fs.PurgeAll())
	if n := sweep(); n != 2 || u.exists(webui.ThumbDir+"/"+v2) || u.exists(webui.ThumbDir+"/"+goneKey) || !u.exists(webui.ThumbDir+"/"+v3) || !u.exists(webui.ThumbDir+"/foreign.jpg") {
		t.Fatalf("sweep after the purge removed %d", n)
	}
	if sweep() != 0 {
		t.Fatal("a second sweep removed something")
	}
}

type tags struct {
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	Album       string `json:"album"`
	AlbumArtist string `json:"album_artist"`
	Track       int    `json:"track"`
	Tracks      int    `json:"tracks"`
	Disc        int    `json:"disc"`
	Year        int    `json:"year"`
	Genre       string `json:"genre"`
	HasCover    bool   `json:"has_cover"`
}

func (u *ui) tagsOf(path string) (int, tags) {
	u.t.Helper()
	code, _, body := u.get("/api/tags?path=" + path)
	var tg tags
	if code == 200 {
		must(u.t, json.Unmarshal(body, &tg))
	}
	return code, tg
}

// /api/tags over ID3v2 with a picture, ID3v1, FLAC comments with a picture, a file
// without tags, a truncated tag and garbage: every audio file is 200, has_cover agrees
// with /api/thumb, and a non-audio file is 400.
//
// Invariant: I-164
func TestWebUI_Tags_Table(t *testing.T) {
	u := newUI(t)
	pic := jpegBytes(64, 64, green)
	audio := testharness.Pattern(4000, 9)
	files := map[string][]byte{
		"/t/id3v2.mp3":   append(id3v2(map[string]string{"TIT2": "Mascara", "TPE1": "Deftones", "TALB": "Around the Fur", "TPE2": "Deftones (album)", "TRCK": "3/10", "TPOS": "1/1", "TYER": "1997", "TCON": "Rock"}, pic), audio...),
		"/t/id3v1.mp3":   append(append([]byte{}, audio...), id3v1("Lhabia", "Deftones", "Around the Fur", "1997", 2)...),
		"/t/flac.flac":   flacFile([]string{"TITLE=Rickets", "ARTIST=Deftones", "ALBUM=Around the Fur", "TRACKNUMBER=5", "DATE=1997", "GENRE=Rock"}, pic),
		"/t/none.mp3":    audio,
		"/t/short.mp3":   append(id3Header(100000), audio[:20]...),
		"/t/garbage.ogg": audio,
		"/t/noise.m4a":   []byte("\x00\x00\x00\x20ftypM4A \x00\x00\x00\x00garbage"),
	}
	for p, b := range files {
		u.write(p, 0, b)
	}
	u.write("/t/note.txt", 0, []byte("text"))
	u.flush()
	want := map[string]tags{
		"/t/id3v2.mp3": {Title: "Mascara", Artist: "Deftones", Album: "Around the Fur", AlbumArtist: "Deftones (album)", Track: 3, Tracks: 10, Disc: 1, Year: 1997, Genre: "Rock", HasCover: true},
		"/t/id3v1.mp3": {Title: "Lhabia", Artist: "Deftones", Album: "Around the Fur", Track: 2, Year: 1997, Genre: "Rock"},
		"/t/flac.flac": {Title: "Rickets", Artist: "Deftones", Album: "Around the Fur", Track: 5, Year: 1997, Genre: "Rock", HasCover: true},
	}
	for p := range files {
		code, got := u.tagsOf(p)
		if code != 200 {
			t.Fatalf("tags %s: %d", p, code)
		}
		if w, ok := want[p]; ok && got != w {
			t.Fatalf("tags %s: %+v, want %+v", p, got, w)
		} else if !ok && got != (tags{}) {
			t.Fatalf("tags %s: %+v, want empty", p, got)
		}
		code, _, _ = u.thumb("path=" + p)
		if (code == 200) != got.HasCover {
			t.Fatalf("thumb %s: %d, has_cover %v", p, code, got.HasCover)
		}
	}
	for _, p := range []string{"/t/note.txt", "/t", "/nope"} {
		if code, _ := u.tagsOf(p); code != 400 && code != 404 {
			t.Fatalf("tags %s: %d", p, code)
		}
	}
}

// A tag read never streams the file: a tag that claims to run past TagHead is given up
// with empty tags, and the chunks fetched stay within the head plus the tail.
//
// Invariant: I-164
func TestWebUI_Tags_ReadBounded(t *testing.T) {
	u := newUI(t)
	cs := int(testharness.ChunkSize)
	body := append(id3Header(11<<20), testharness.Pattern(12<<20, 3)...)
	u.write("/long.mp3", 0, body)
	u.flush()
	before := 0
	for n := meta.NodeIdx(1); n <= 3; n++ {
		before += u.f.Gets(n)
	}
	code, got := u.tagsOf("/long.mp3")
	if code != 200 || got != (tags{}) {
		t.Fatalf("tags: %d %+v", code, got)
	}
	after := 0
	for n := meta.NodeIdx(1); n <= 3; n++ {
		after += u.f.Gets(n)
	}
	// k = 2 shards per chunk, the head's chunks plus the tail's and one of slack
	if limit := 2 * (webui.TagHead/cs + 2); after-before > limit {
		t.Fatalf("%d shard reads for a bounded tag read, want at most %d", after-before, limit)
	}
}

// An audio file's thumbnail is its embedded picture, else cover.* before folder.*,
// each scaled like an image; with none it has no thumbnail.
//
// Invariant: I-165
func TestWebUI_Thumb_AudioCoverFallbackOrder(t *testing.T) {
	u := newUI(t)
	audio := testharness.Pattern(2000, 1)
	u.write("/m/plain.mp3", 0, append(id3v2(map[string]string{"TIT2": "x"}, nil), audio...))
	u.write("/m/embedded.mp3", 0, append(id3v2(map[string]string{"TIT2": "y"}, jpegBytes(64, 64, green)), audio...))
	u.write("/m/folder.jpg", 0, jpegBytes(400, 200, blue))
	u.write("/m/cover.png", 0, pngBytes(400, 200, red))
	u.flush()
	thumbHue := func(p string) string {
		t.Helper()
		code, _, body := u.thumb("path=" + p)
		if code != 200 {
			t.Fatalf("thumb %s: %d", p, code)
		}
		img := decodeJPEG(t, body)
		if img.Bounds().Dx() > webui.ThumbWidth {
			t.Fatalf("thumb %s is %d wide", p, img.Bounds().Dx())
		}
		return hue(img)
	}
	if h := thumbHue("/m/embedded.mp3"); h != "green" {
		t.Fatalf("embedded picture: %s", h)
	}
	if h := thumbHue("/m/plain.mp3"); h != "red" {
		t.Fatalf("cover.png before folder.jpg: %s", h)
	}
	u.del("/m/cover.png")
	u.write("/m/other.mp3", 0, append(id3v2(map[string]string{"TIT2": "z"}, nil), audio...))
	u.flush()
	if h := thumbHue("/m/other.mp3"); h != "blue" {
		t.Fatalf("folder.jpg: %s", h)
	}
	u.del("/m/folder.jpg")
	u.write("/m/last.mp3", 0, append(id3v2(map[string]string{"TIT2": "w"}, nil), audio...))
	u.flush()
	if code, _, _ := u.thumb("path=/m/last.mp3"); code != 404 {
		t.Fatalf("no picture anywhere: %d", code)
	}
}

// A folder's cover is its first audio entry by name that has a picture, absent when
// none has one, and the same on every node of a cluster.
//
// Invariant: I-165
func TestWebUI_Ls_DirCoverDeterministic(t *testing.T) {
	u := newUI(t)
	pic := jpegBytes(16, 16, green)
	audio := testharness.Pattern(500, 1)
	u.write("/alb/a.mp3", 0, append(id3v2(map[string]string{"TIT2": "a"}, nil), audio...))
	u.write("/alb/b.mp3", 0, append(id3v2(map[string]string{"TIT2": "b"}, pic), audio...))
	u.write("/alb/c.mp3", 0, append(id3v2(map[string]string{"TIT2": "c"}, pic), audio...))
	u.write("/plain/a.mp3", 0, append(id3v2(map[string]string{"TIT2": "a"}, nil), audio...))
	u.flush()
	// a listing reports a cover only once the folder's thumbnail resolved it (I-165)
	if l := u.lsOf("path=/alb"); l.Dir.Cover != "" {
		t.Fatalf("cover of /alb before any thumbnail request: %q", l.Dir.Cover)
	}
	if code, _, _ := u.get("/api/thumb?path=/alb"); code != 200 {
		t.Fatalf("folder thumb: %d", code)
	}
	if code, _, _ := u.get("/api/thumb?path=/plain"); code != 404 {
		t.Fatalf("folder thumb without a picture: %d, want 404", code)
	}
	if l := u.lsOf("path=/alb"); l.Dir.Cover != "b.mp3" {
		t.Fatalf("cover of /alb: %q", l.Dir.Cover)
	}
	root := u.lsOf("path=/")
	if root.entry("alb").Cover != "b.mp3" || root.entry("plain").Cover != "" {
		t.Fatalf("covers in the root listing: %q %q", root.entry("alb").Cover, root.entry("plain").Cover)
	}
	u.write("/alb/0.mp3", 0, append(id3v2(map[string]string{"TIT2": "0"}, pic), audio...))
	u.flush()
	u.get("/api/thumb?path=/alb")
	if l := u.lsOf("path=/alb"); l.Dir.Cover != "0.mp3" {
		t.Fatalf("cover after adding 0.mp3: %q", l.Dir.Cover)
	}

	// three nodes of one cluster answer the same cover
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	c.WriteFile(0, "/alb/a.mp3", append(id3v2(map[string]string{"TIT2": "a"}, nil), audio...))
	c.WriteFile(0, "/alb/b.mp3", append(id3v2(map[string]string{"TIT2": "b"}, pic), audio...))
	c.Settle()
	for i := range c.Nodes {
		ts := tailscale.NewFake(loop, stableS, nil)
		auth := &rpc.WhoIsAuthorizer{TS: ts, Clk: c.Clk, Member: func(string) (meta.Node, bool) {
			return meta.Node{Idx: 1, ID: "n1", Name: "one", Kind: meta.KindDevice, TSStableID: stableS, Addr: loop.String()}, true
		}}
		srv, err := webui.New(webui.Options{FS: c.Nodes[i].FS, Store: c.Nodes[i].Node.FSM, Auth: auth, Clk: c.Clk, Logger: slog.New(slog.DiscardHandler)})
		must(t, err)
		thumb := httptest.NewRecorder()
		treq := httptest.NewRequest(http.MethodGet, "/api/thumb?path=/alb", http.NoBody)
		treq.RemoteAddr = loop.String() + ":4242"
		srv.Handler().ServeHTTP(thumb, treq)
		if thumb.Code != 200 {
			t.Fatalf("n%d: folder thumb %d %s", i, thumb.Code, strings.TrimSpace(thumb.Body.String()))
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/ls?path=/", http.NoBody)
		req.RemoteAddr = loop.String() + ":4242"
		srv.Handler().ServeHTTP(rec, req)
		var l listing
		must(t, json.Unmarshal(rec.Body.Bytes(), &l))
		if rec.Code != 200 || l.entry("alb") == nil || l.entry("alb").Cover != "b.mp3" {
			t.Fatalf("n%d: %d %s", i, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// A listing reads no file: the covers of a folder of albums cost no shard GET until a
// folder's thumbnail is asked for, which resolves and remembers that folder's cover.
// Invariant: I-165
func TestWebUI_Ls_ReadsNoFile(t *testing.T) {
	u := newUI(t)
	pic := jpegBytes(16, 16, green)
	audio := testharness.Pattern(500, 1)
	for i := range 20 {
		u.write(fmt.Sprintf("/music/alb%02d/a.mp3", i), 0, append(id3v2(map[string]string{"TIT2": "a"}, pic), audio...))
	}
	u.flush()
	gets := func() int {
		n := 0
		for _, tgt := range u.f.Targets() {
			n += u.f.Gets(tgt)
		}
		return n
	}
	before := gets()
	l := u.lsOf("path=/music")
	if len(l.Entries) != 20 {
		t.Fatalf("%d entries", len(l.Entries))
	}
	for _, e := range l.Entries {
		if e.Cover != "" {
			t.Fatalf("%s carries a cover before any thumbnail request", e.Name)
		}
	}
	if n := gets(); n != before {
		t.Fatalf("the listing issued %d shard GETs", n-before)
	}
	if code, _, _ := u.get("/api/thumb?path=/music/alb07"); code != 200 {
		t.Fatalf("folder thumb: %d", code)
	}
	if n := gets(); n <= before {
		t.Fatal("the folder thumbnail read nothing")
	}
	l = u.lsOf("path=/music")
	for _, e := range l.Entries {
		if want := ""; e.Name == "alb07" {
			want = "a.mp3"
			if e.Cover != want {
				t.Fatalf("alb07 cover %q after its thumbnail, want %q", e.Cover, want)
			}
		} else if e.Cover != "" {
			t.Fatalf("%s carries a cover nobody resolved", e.Name)
		}
	}
}
