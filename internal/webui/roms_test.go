package webui_test

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/webui"
)

// romsUI is a media UI with a roms library laid out as a real one.
func romsUI(t *testing.T) (*ui, func()) {
	t.Helper()
	u, pass := mediaUI(t)
	for _, f := range []string{
		"/roms/SNES/USA/Donkey Kong Country (USA) (Rev 2).zip",
		"/roms/SNES/USA/Donkey Kong Country (USA).zip",
		"/roms/SNES/USA/Batman - Revenge of the Joker (USA) (Proto).zip",
		"/roms/SNES/the password is skwbr.txt",
		"/roms/Gamecube/Tom Clancy's Splinter Cell - Chaos Theory (Disc 1).iso",
		"/roms/Gamecube/Tom Clancy's Splinter Cell - Chaos Theory (Disc 2).iso",
		"/roms/Switch/Games/Borderlands 3 [01009970122E4000][USA][v0].xci",
		"/roms/Switch/Games/Borderlands 3 [01009970122E4800][USA][v196608].nsp",
		"/roms/Switch/emulator.apk",
	} {
		u.write(f, 3, []byte("rom")) // a non-zero offset
	}
	u.write("/roms/SNES/gamelist.xml", 0, []byte(`<gameList><game><path>./USA/Donkey Kong Country (USA) (Rev 2).zip</path><name>Donkey Kong Country</name><desc>Apes.</desc><developer>Rare</developer><releasedate>19941121</releasedate></game></gameList>`))
	u.flush()
	if code, _ := u.setFlags("/roms", `"library":"roms"`); code != 200 {
		t.Fatalf("flag /roms: %d", code)
	}
	return u, pass
}

// A roms library lists its platforms live (display name, game count by title); a
// platform lists one card per title, its other dumps counted, with regions, revision and
// flags; a game lists its discs and its other dumps as files; the gamelist's fields fill
// in after a pass; nothing a platform does not take is a game (docs/13 §10).
// Invariant: I-279
func TestWebUI_Media_Roms_Table(t *testing.T) {
	u, pass := romsUI(t)
	var lib mediaLib
	if code := u.mediaJSON("lib=/roms", &lib); code != 200 {
		t.Fatalf("lib: %d", code)
	}
	got := map[string]int{}
	for _, it := range lib.Items {
		if it.Kind != "platform" {
			t.Fatalf("a roms library lists %s %q", it.Kind, it.Title)
		}
		got[it.Title] = it.Children
	}
	if want := map[string]int{"Super Nintendo": 2, "GameCube": 1, "Nintendo Switch": 1}; !reflect.DeepEqual(got, want) || lib.Lib.Kind != "roms" {
		t.Fatalf("platforms %v, want %v", got, want)
	}
	var snes webui.MediaItem
	if code := u.mediaJSON("item=/roms/SNES", &snes); code != 200 || snes.Kind != "platform" || len(snes.Games) != 2 {
		t.Fatalf("snes: %d %+v", code, snes)
	}
	dk := snes.Games[1]
	if snes.Games[0].Title != "Batman - Revenge of the Joker" || !reflect.DeepEqual(snes.Games[0].Flags, []string{"proto"}) ||
		dk.Title != "Donkey Kong Country" || dk.Variants != 1 || !reflect.DeepEqual(dk.Regions, []string{"USA"}) {
		t.Fatalf("snes games %+v", snes.Games)
	}
	var game webui.MediaItem
	if code := u.mediaJSON("item="+url.QueryEscape(dk.Path), &game); code != 200 || game.Kind != "game" || len(game.Files) != 2 || game.Platform != "Super Nintendo" {
		t.Fatalf("game: %d %+v", code, game)
	}
	if game.Plot != "" {
		t.Fatal("the gamelist was read before a pass")
	}
	pass()
	u.mediaJSON("item="+url.QueryEscape("/roms/SNES/USA/Donkey Kong Country (USA) (Rev 2).zip"), &game)
	if game.Plot != "Apes." || game.Developer != "Rare" || game.Year != 1994 {
		t.Fatalf("after a pass: %+v", game)
	}
	var gc webui.MediaItem
	u.mediaJSON("item="+url.QueryEscape("/roms/Gamecube/Tom Clancy's Splinter Cell - Chaos Theory (Disc 1).iso"), &gc)
	if gc.Discs != 2 || len(gc.Files) != 2 || gc.Files[0].Disc != 1 || gc.Files[1].Disc != 2 {
		t.Fatalf("a two-disc game: %+v", gc)
	}
	if code := u.mediaJSON("item=/roms/SNES/the%20password%20is%20skwbr.txt", nil); code != 404 {
		t.Fatalf("a text file is a game: %d", code)
	}
}

// Nothing in the Media tab plays or launches a game: a game carries no video, its files
// link only to the file route, and no launch route exists (docs/13 §10).
// Invariant: I-279
func TestWebUI_Media_NoLaunchRoute(t *testing.T) {
	u, _ := romsUI(t)
	var game webui.MediaItem
	u.mediaJSON("item="+url.QueryEscape("/roms/Switch/Games/Borderlands 3 [01009970122E4000][USA][v0].xci"), &game)
	if game.Kind != "game" || game.Video != "" || game.VideoIno != 0 {
		t.Fatalf("a game carries a video to play: %+v", game)
	}
	for _, f := range game.Files {
		if !strings.HasPrefix(f.Path, "/roms/") {
			t.Fatalf("file %+v", f)
		}
	}
	for _, route := range []string{"/api/launch", "/api/play", "/api/emulate", "/api/run"} {
		if code, _, _ := u.get(route + "?path=" + url.QueryEscape(game.Path)); code != http.StatusNotFound {
			t.Fatalf("%s: %d, want 404", route, code)
		}
	}
}

// A folder under a library without a known cover carries a title card's words in the
// listing, from its name alone, for its tile to draw when no art arrives (a folder of
// video no browser can frame never settles): a show's analysed title and year, a
// platform's display name and folder name; a folder with a cover or outside a library
// carries none.
// Invariant: I-280
func TestWebUI_Ls_TitleCardFields(t *testing.T) {
	u, _ := romsUI(t)
	u.write("/tv/Silo 2023 Seasons 1 and 2 Complete 1080p WEB x264 [i_c]/notes.txt", 0, []byte("n"))
	u.write("/tv/Unasked/notes.txt", 0, []byte("n"))
	u.write("/plain/Folder/notes.txt", 0, []byte("n"))
	u.write("/roms/N64/notes.txt", 0, []byte("n"))
	u.flush()
	if code, _ := u.setFlags("/tv", `"library":"tv"`); code != 200 {
		t.Fatal("flag /tv")
	}
	for _, p := range []string{"/tv/Silo 2023 Seasons 1 and 2 Complete 1080p WEB x264 [i_c]", "/roms/N64", "/roms/SNES", "/plain/Folder"} {
		u.get("/api/thumb?path=" + url.QueryEscape(p)) // resolve each to no cover
	}
	tv := u.lsOf("path=/tv")
	if c := tv.entry("Silo 2023 Seasons 1 and 2 Complete 1080p WEB x264 [i_c]").Card; c == nil || *c != (webui.Card{Title: "Silo", Sub: "2023"}) {
		t.Fatalf("show card %+v", c)
	}
	if c := tv.entry("Unasked").Card; c == nil || c.Title != "Unasked" {
		t.Fatalf("an unresolved folder's card: %+v", c)
	}
	u.write("/tv/Covered/cover.jpg", 0, jpegBytes(16, 16, green))
	u.flush()
	u.get("/api/thumb?path=/tv/Covered")
	if c := u.lsOf("path=/tv").entry("Covered").Card; c == nil || c.Title != "Covered" {
		t.Fatalf("a folder with a cover carries its card for the overlay: %+v", c)
	}
	roms := u.lsOf("path=/roms")
	if c := roms.entry("N64").Card; c == nil || *c != (webui.Card{Title: "Nintendo 64", Sub: "N64"}) {
		t.Fatalf("platform card %+v", c)
	}
	if c := u.lsOf("path=/plain").entry("Folder").Card; c != nil {
		t.Fatalf("a folder outside a library has a card: %+v", c)
	}
}

// A platform's card shows its first game's box art when its folder has none, as a volume
// path, and a catalogue made before the art was written does not hide it (docs/13 §10).
// Invariant: I-400
func TestWebUI_Media_PlatformArt(t *testing.T) {
	u, pass := romsUI(t)
	pass() // the catalogue first: it holds no art for SNES yet
	u.write("/roms/SNES/USA/Batman - Revenge of the Joker (USA) (Proto).png", 3, []byte("png"))
	u.flush()
	var lib mediaLib
	if code := u.mediaJSON("lib=/roms", &lib); code != 200 {
		t.Fatalf("lib: %d", code)
	}
	posters := map[string]string{}
	for _, it := range lib.Items {
		posters[it.Title] = it.Poster
	}
	if want := "/roms/SNES/USA/Batman - Revenge of the Joker (USA) (Proto).png"; posters["Super Nintendo"] != want {
		t.Fatalf("SNES poster %q, want %q", posters["Super Nintendo"], want)
	}
	if posters["GameCube"] != "" {
		t.Fatalf("GameCube has no art, yet poster %q", posters["GameCube"])
	}
}
