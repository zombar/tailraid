package webui_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/webui"
)

// putFrame stores a frame for the video at p as the browser does.
func (u *ui) putFrame(p string) {
	u.t.Helper()
	in := u.statOf(p)
	resp := u.do(http.MethodPut, fmt.Sprintf("/api/thumb?path=%s&version=%d", url.QueryEscape(p), in.Version), bytes.NewReader(jpegBytes(32, 32, green)), webui.HeaderUI, "1")
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		u.t.Fatalf("storing the frame of %s: %d", p, resp.StatusCode)
	}
	u.flush()
}

// folderThumb asks for a folder's tile and decodes a 404's grab hint.
func (u *ui) folderThumb(p string) (int, webui.Grab) {
	u.t.Helper()
	code, _, body := u.get("/api/thumb?path=" + url.QueryEscape(p))
	var miss struct {
		Grab webui.Grab `json:"grab"`
	}
	if code == http.StatusNotFound {
		must(u.t, json.Unmarshal(body, &miss))
	}
	return code, miss.Grab
}

// A folder's cover comes from what its media really is on a volume (docs/13 §5): a folder
// of photos takes its first photo, a tracker's advert image never counts, a Subs folder is
// not looked into, and media four levels down still surfaces. Before, only cover.*,
// folder.* and poster.* images counted, so a folder of photos and every folder above it
// had no tile, and the YTS advert beside a film would have been its poster.
// Invariant: I-275
func TestWebUI_DirCover_PhotosAndAdverts(t *testing.T) {
	u := newUI(t)
	pic := jpegBytes(16, 16, green)
	audio := testharness.Pattern(500, 1)
	u.write("/Photos/2024/Trip/IMG_0002.jpg", 0, jpegBytes(64, 64, red))
	u.write("/Photos/2024/Trip/IMG_0001.jpg", 0, jpegBytes(64, 64, blue))
	u.write("/Films/Upgrade (2018) [YTS.AM]/www.YTS.AM.jpg", 0, jpegBytes(64, 64, red))
	u.write("/Films/Upgrade (2018) [YTS.AM]/YTS.BZ - Official site.jpg", 0, jpegBytes(64, 64, red))
	u.write("/Films/Upgrade (2018) [YTS.AM]/Upgrade.2018.1080p.mp4", 0, []byte("video"))
	u.write("/Films/Upgrade (2018) [YTS.AM]/Subs/English.srt", 0, []byte("1"))
	u.write("/Show/Subs/x/still.jpg", 0, jpegBytes(64, 64, red))
	u.write("/Music/Artist/1 Studio/1983 90125/CD1/a.mp3", 0, append(id3v2(map[string]string{"TIT2": "a"}, pic), audio...))
	u.write("/Far/a/b/c/d/e/x.mp3", 0, append(id3v2(map[string]string{"TIT2": "x"}, pic), audio...))
	u.flush()

	cases := []struct {
		path, cover string
		code        int
	}{
		{"/Photos/2024/Trip", "IMG_0001.jpg", 200},              // the first photo by name
		{"/Photos", "2024/Trip/IMG_0001.jpg", 200},              // and it surfaces
		{"/Films/Upgrade (2018) [YTS.AM]", "", 404},             // adverts are not art
		{"/Show", "", 404},                                      // nor is a Subs folder's content
		{"/Music", "Artist/1 Studio/1983 90125/CD1/a.mp3", 200}, // four levels: within CoverDepth
		{"/Far", "", 404},                                       // five levels: past it
	}
	for _, c := range cases {
		if code, _ := u.folderThumb(c.path); code != c.code {
			t.Fatalf("thumb %s: %d, want %d", c.path, code, c.code)
		}
		if l := u.lsOf("path=" + url.QueryEscape(c.path)); l.Dir.Cover != c.cover {
			t.Fatalf("%s: cover %q, want %q", c.path, l.Dir.Cover, c.cover)
		}
	}
	if code, _, body := u.thumb("path=/Photos"); code != 200 || hue(decodeJPEG(t, body)) != "blue" {
		t.Fatalf("/Photos serves %d, not IMG_0001", code)
	}
}

// A folder whose only media is video nobody has framed yet is not a settled "none": the
// 404 names the video to grab, the listing does not mark the folder no_cover, and once a
// frame is stored every node resolves the folder again and finds it, however far up. An
// audio file written after the folder was resolved is looked at again too. Before, a
// "none" was stored for every node with only folder versions as its dependencies, so a
// frame stored later, or a track's bytes written after its create, never cleared it: the
// TV shows of a real volume stayed no_cover after their episodes had frames.
// Invariant: I-275
func TestWebUI_DirCover_NoneClears(t *testing.T) {
	u := newUI(t)
	pic := jpegBytes(16, 16, green)
	audio := testharness.Pattern(500, 1)
	u.write("/TV/Silo/Season 1/Silo - S01E01 - Freedom Day.mkv", 0, []byte("video"))
	u.write("/TV/Silo/Season 1/Silo - S01E02 - Machines.mp4", 0, []byte("video"))
	u.write("/Late/Album/t.mp3", 0, nil)
	u.flush()

	// the 404 names a video the browser can decode before an mkv
	code, g := u.folderThumb("/TV")
	if code != 404 || g.Path != "Silo/Season 1/Silo - S01E02 - Machines.mp4" || g.Ino == 0 {
		t.Fatalf("thumb /TV before a frame: %d %+v, want 404 with the mp4 to grab", code, g)
	}
	if want := u.statOf("/TV/Silo/Season 1/Silo - S01E02 - Machines.mp4"); g.Ino != want.Ino || g.Version != want.Version {
		t.Fatalf("grab names ino %d v%d, want %d v%d", g.Ino, g.Version, want.Ino, want.Version)
	}
	u.flush()
	if l := u.lsOf("path=/TV"); l.Dir.NoCover {
		t.Fatal("a folder waiting for a frame is marked no_cover")
	}
	if code, _ := u.folderThumb("/Late"); code != 404 {
		t.Fatalf("thumb /Late with an empty track: %d", code)
	}
	u.flush()

	// the browser stores the frame; another node sees the folder resolve
	u.putFrame("/TV/Silo/Season 1/Silo - S01E02 - Machines.mp4")
	other := u.node()
	if code, _ := other.folderThumb("/TV"); code != 200 {
		t.Fatalf("thumb /TV after the frame, another node: %d", code)
	}
	if l := other.lsOf("path=/TV"); l.Dir.Cover != "Silo/Season 1/Silo - S01E02 - Machines.mp4" {
		t.Fatalf("/TV cover after the frame: %q", l.Dir.Cover)
	}

	// the track's bytes arrive after its create, the tail at a non-zero offset
	head := id3v2(map[string]string{"TIT2": "t"}, pic)
	u.write("/Late/Album/t.mp3", int64(len(head)), audio)
	u.write("/Late/Album/t.mp3", 0, head)
	u.flush()
	if code, _ := u.node().folderThumb("/Late"); code != 200 {
		t.Fatalf("thumb /Late after the track was written: %d", code)
	}
}

// A "none" stored by an older binary lists no videos it waits for and so cannot know a
// frame arrived: it is resolved again, while a stored choice of a file is still served.
// Invariant: I-275
func TestWebUI_DirCover_LegacyNoneResolvedAgain(t *testing.T) {
	u := newUI(t)
	u.write("/Show/ep.mp4", 0, []byte("video"))
	u.flush()
	in, err := u.f.FSM.Resolve(meta.SplitPath("/Show"))
	must(t, err)
	legacy, err := json.Marshal(map[string]any{"name": "", "deps": []map[string]any{{"ino": in.Ino, "version": in.Version, "mtime": in.Mtime}}})
	must(t, err)
	must(t, u.fs.MkdirAll(webui.ThumbDir, 0o755))
	fh, err := u.fs.OpenFile(path.Join(webui.ThumbDir, fmt.Sprintf("%d-%d.cover", in.Ino, in.Gen)), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	must(t, err)
	_, err = fh.Write(legacy)
	must(t, err)
	must(t, fh.Close())
	u.putFrame("/Show/ep.mp4")
	if code, _ := u.node().folderThumb("/Show"); code != 200 {
		t.Fatalf("a legacy none record kept /Show without its frame: %d", code)
	}
}
