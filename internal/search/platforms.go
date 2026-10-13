package search

import (
	"bytes"
	"encoding/xml"
	"path"
	"slices"
	"strings"
)

// platform is one entry of the platform table (docs/18 §8b): the folder names that mean
// it (EmulationStation's short names and the common spellings, compared folded), its
// display name, the extensions a game of it has and its system's name in the libretro
// thumbnail set ("" none), where `media scrape` finds box art (docs/18 §11).
type platform struct {
	names    []string
	title    string
	exts     []string
	libretro string
}

// platforms is the table. A folder no entry names keeps its own name and takes any
// game extension of romExts.
var platforms = []platform{
	{[]string{"nes", "famicom"}, "Nintendo Entertainment System", []string{".nes", ".fds", ".unf", ".zip", ".7z"}, "Nintendo - Nintendo Entertainment System"},
	{[]string{"snes", "sfc", "super nintendo", "superfamicom"}, "Super Nintendo", []string{".sfc", ".smc", ".zip", ".7z"}, "Nintendo - Super Nintendo Entertainment System"},
	{[]string{"n64", "nintendo 64"}, "Nintendo 64", []string{".z64", ".n64", ".v64", ".zip", ".7z"}, "Nintendo - Nintendo 64"},
	{[]string{"gb", "gameboy", "game boy"}, "Game Boy", []string{".gb", ".zip", ".7z"}, "Nintendo - Game Boy"},
	{[]string{"gbc", "gameboy color", "game boy color"}, "Game Boy Color", []string{".gbc", ".zip", ".7z"}, "Nintendo - Game Boy Color"},
	{[]string{"gba", "gameboy advance", "game boy advance"}, "Game Boy Advance", []string{".gba", ".zip", ".7z"}, "Nintendo - Game Boy Advance"},
	{[]string{"nds", "ds", "nintendo ds"}, "Nintendo DS", []string{".nds", ".zip", ".7z"}, "Nintendo - Nintendo DS"},
	{[]string{"3ds", "n3ds", "nintendo 3ds"}, "Nintendo 3DS", []string{".3ds", ".cia", ".cci"}, "Nintendo - Nintendo 3DS"},
	{[]string{"gc", "gamecube", "ngc"}, "GameCube", []string{".iso", ".rvz", ".gcz", ".ciso", ".gcm"}, "Nintendo - GameCube"},
	{[]string{"wii"}, "Wii", []string{".iso", ".wbfs", ".rvz", ".ciso"}, "Nintendo - Wii"},
	{[]string{"wiiu", "wii u"}, "Wii U", []string{".wua", ".wud", ".wux", ".rpx"}, "Nintendo - Wii U"},
	{[]string{"switch", "nsw"}, "Nintendo Switch", []string{".nsp", ".xci", ".nsz", ".xcz"}, ""},
	{[]string{"megadrive", "genesis", "mega drive", "md"}, "Mega Drive", []string{".md", ".gen", ".smd", ".bin", ".zip", ".7z"}, "Sega - Mega Drive - Genesis"},
	{[]string{"mastersystem", "sms", "master system"}, "Master System", []string{".sms", ".zip", ".7z"}, "Sega - Master System - Mark III"},
	{[]string{"gamegear", "gg", "game gear"}, "Game Gear", []string{".gg", ".zip", ".7z"}, "Sega - Game Gear"},
	{[]string{"segacd", "megacd", "sega cd", "mega cd"}, "Sega CD", []string{".cue", ".chd", ".iso", ".m3u"}, "Sega - Mega-CD - Sega CD"},
	{[]string{"32x", "sega32x"}, "32X", []string{".32x", ".zip", ".7z"}, "Sega - 32X"},
	{[]string{"saturn"}, "Saturn", []string{".cue", ".chd", ".iso", ".m3u"}, "Sega - Saturn"},
	{[]string{"dreamcast", "dc"}, "Dreamcast", []string{".gdi", ".cdi", ".chd", ".m3u"}, "Sega - Dreamcast"},
	{[]string{"psx", "ps1", "playstation", "ps"}, "PlayStation", []string{".pbp", ".chd", ".cue", ".m3u", ".iso"}, "Sony - PlayStation"},
	{[]string{"ps2", "playstation 2"}, "PlayStation 2", []string{".iso", ".chd", ".cso"}, "Sony - PlayStation 2"},
	{[]string{"psp", "playstation portable"}, "PlayStation Portable", []string{".iso", ".cso", ".pbp", ".chd"}, "Sony - PlayStation Portable"},
	{[]string{"pcengine", "pce", "tg16", "turbografx16", "turbografx-16"}, "PC Engine", []string{".pce", ".cue", ".chd", ".zip", ".7z"}, "NEC - PC Engine - TurboGrafx 16"},
	{[]string{"atari2600", "2600"}, "Atari 2600", []string{".a26", ".bin", ".zip", ".7z"}, "Atari - 2600"},
	{[]string{"atari7800", "7800"}, "Atari 7800", []string{".a78", ".zip", ".7z"}, "Atari - 7800"},
	{[]string{"lynx", "atarilynx"}, "Lynx", []string{".lnx", ".zip", ".7z"}, "Atari - Lynx"},
	{[]string{"neogeo", "neo geo"}, "Neo Geo", []string{".zip", ".7z"}, "SNK - Neo Geo"},
	{[]string{"arcade", "mame", "fbneo", "fba"}, "Arcade", []string{".zip", ".7z"}, "MAME"},
	{[]string{"msx"}, "MSX", []string{".rom", ".mx1", ".mx2", ".dsk", ".zip"}, "Microsoft - MSX"},
	{[]string{"c64", "commodore 64"}, "Commodore 64", []string{".d64", ".t64", ".prg", ".crt", ".zip"}, "Commodore - 64"},
	{[]string{"amiga"}, "Amiga", []string{".adf", ".lha", ".hdf", ".zip"}, "Commodore - Amiga"},
	{[]string{"dos", "pc"}, "DOS", []string{".zip", ".exe", ".dosz"}, "DOS"},
}

// platformOf is the table's entry for a platform folder, or one that keeps the folder's
// name and takes any game extension.
func platformOf(dir string) platform {
	f := strings.ToLower(strings.TrimSpace(dir))
	for i := range platforms {
		if slices.Contains(platforms[i].names, f) {
			return platforms[i]
		}
	}
	exts := make([]string, 0, len(romExts))
	for e := range romExts {
		exts = append(exts, e)
	}
	return platform{title: strings.TrimSpace(dir), exts: exts}
}

// Libretro is the libretro thumbnail system of a platform's display name, "" when the
// set has none or the platform is not in the table.
func Libretro(title string) string {
	for i := range platforms {
		if platforms[i].title == title {
			return platforms[i].libretro
		}
	}
	return ""
}

// PlatformTitle is a platform folder's display name: the table's, else the name itself
// (docs/18 §8b).
func PlatformTitle(dir string) string { return platformOf(dir).title }

func (p platform) takes(name string) bool {
	return slices.Contains(p.exts, ext(name))
}

// setExts mark a folder that is one game: a disc image set (docs/18 §8b).
var setExts = map[string]bool{".cue": true, ".m3u": true, ".gdi": true}

// GameInfo is one game of an EmulationStation gamelist.xml (docs/18 §8b).
type GameInfo struct {
	Name, Desc, Developer, Publisher, Genre, Players, Rating string
	Year                                                     int
	Image, Thumbnail, Marquee                                string // paths relative to the platform folder
}

type gamelistXML struct {
	XMLName xml.Name `xml:"gameList"`
	Games   []struct {
		Path        string `xml:"path"`
		Name        string `xml:"name"`
		Desc        string `xml:"desc"`
		ReleaseDate string `xml:"releasedate"`
		Developer   string `xml:"developer"`
		Publisher   string `xml:"publisher"`
		Genre       string `xml:"genre"`
		Players     string `xml:"players"`
		Rating      string `xml:"rating"`
		Image       string `xml:"image"`
		Thumbnail   string `xml:"thumbnail"`
		Marquee     string `xml:"marquee"`
	} `xml:"game"`
}

// DecodeGamelist reads a gamelist.xml into its games by path, the path cleaned and
// relative to the platform folder ("./USA/x.zip" is "USA/x.zip"); one that does not
// parse is nothing (docs/18 §8b).
func DecodeGamelist(b []byte) (map[string]GameInfo, bool) {
	var g gamelistXML
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	if err := d.Decode(&g); err != nil {
		return nil, false
	}
	out := make(map[string]GameInfo, len(g.Games))
	for i := range g.Games {
		x := &g.Games[i]
		p := relPath(x.Path)
		if p == "" {
			continue
		}
		gi := GameInfo{
			Name: strings.TrimSpace(x.Name), Desc: strings.TrimSpace(x.Desc), Developer: strings.TrimSpace(x.Developer),
			Publisher: strings.TrimSpace(x.Publisher), Genre: strings.TrimSpace(x.Genre), Players: strings.TrimSpace(x.Players),
			Rating: strings.TrimSpace(x.Rating), Image: relPath(x.Image), Thumbnail: relPath(x.Thumbnail), Marquee: relPath(x.Marquee),
		}
		if len(x.ReleaseDate) >= 4 && yearTok.MatchString(x.ReleaseDate[:4]) {
			gi.Year = atoi(x.ReleaseDate[:4])
		}
		out[p] = gi
	}
	return out, true
}

// relPath cleans a gamelist path to one relative to the platform folder; an absolute or
// escaping path is "".
func relPath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") {
		return ""
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return ""
	}
	return c
}
