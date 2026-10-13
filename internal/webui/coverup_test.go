package webui_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/webui"
)

// Album, show and movie art surfaces to the parent folders, first wins: a folder without
// art of its own takes the cover of its first subfolder by name that has one, looking up
// to CoverDepth levels down; a folder's own art is a track's picture (I-165), else a
// cover.*, folder.* or poster.* image, else the first video by name whose frame is
// stored. Before, only audio counted and only one level down, so a Movies or TV folder,
// and a Music folder of artists, had no tile.
// Invariant: I-257
func TestWebUI_DirCover_SurfacesArtUpward(t *testing.T) {
	u := newUI(t)
	pic := jpegBytes(16, 16, green)
	audio := testharness.Pattern(500, 1)
	u.write("/Movies/Alien (1979)/Alien.mkv", 0, []byte("video"))
	u.write("/Movies/Alien (1979)/poster.jpg", 0, jpegBytes(64, 64, blue))
	u.write("/Movies/Brazil/folder.jpg", 0, jpegBytes(64, 64, red))
	u.write("/TV/ShowA/S01/ep1.mkv", 0, []byte("video"))
	u.write("/TV/ShowB/poster.png", 0, pngBytes(64, 64, red))
	u.write("/Music/Artist/Album/a.mp3", 0, append(id3v2(map[string]string{"TIT2": "a"}, pic), audio...))
	u.write("/Own/poster.jpg", 0, jpegBytes(64, 64, blue))
	u.write("/Own/Sub/cover.jpg", 0, jpegBytes(64, 64, red))
	u.write("/Too/a/b/c/d/e/x.mp3", 0, append(id3v2(map[string]string{"TIT2": "x"}, pic), audio...))
	u.flush()
	// the browser stored ep1's frame
	in := u.statOf("/TV/ShowA/S01/ep1.mkv")
	frame := jpegBytes(32, 32, green)
	resp := u.do(http.MethodPut, fmt.Sprintf("/api/thumb?path=%s&version=%d", "/TV/ShowA/S01/ep1.mkv", in.Version), bytes.NewReader(frame), webui.HeaderUI, "1")
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("storing the frame: %d", resp.StatusCode)
	}
	u.flush()

	cases := []struct {
		path, cover string
		code        int
	}{
		{"/Movies/Alien (1979)", "poster.jpg", 200}, // an artwork image
		{"/Movies", "Alien (1979)/poster.jpg", 200}, // first wins
		{"/TV/ShowA", "S01/ep1.mkv", 200},           // a stored video frame
		{"/TV", "ShowA/S01/ep1.mkv", 200},           // two levels up
		{"/Music", "Artist/Album/a.mp3", 200},       // an artist folder of albums
		{"/Own", "poster.jpg", 200},                 // own art beats a subfolder's
		{"/Too", "", 404},                           // five levels down: past CoverDepth
	}
	for _, c := range cases {
		q := "path=" + url.QueryEscape(c.path)
		if code, _, _ := u.get("/api/thumb?" + q); code != c.code {
			t.Fatalf("thumb %s: %d, want %d", c.path, code, c.code)
		}
		if l := u.lsOf(q); l.Dir.Cover != c.cover || l.Dir.NoCover != (c.cover == "") {
			t.Fatalf("%s: cover %q no_cover %v, want %q", c.path, l.Dir.Cover, l.Dir.NoCover, c.cover)
		}
	}
	if code, _, body := u.thumb("path=/Movies"); code != 200 || hue(decodeJPEG(t, body)) != "blue" {
		t.Fatalf("/Movies serves %d, not Alien's poster", code)
	}
	if code, _, body := u.thumb("path=/TV"); code != 200 || hue(decodeJPEG(t, body)) != "green" {
		t.Fatalf("/TV serves %d, not ep1's frame", code)
	}
}
