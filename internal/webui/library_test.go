package webui_test

import "testing"

// POST /api/flags sets a folder's library kind by word and refuses the rest; ls and stat
// carry the entry's own kind and the folder's effective kind, the nearest flagged
// ancestor's; a file, the root and a bad word are 400.
// Invariant: I-210, I-277
func TestWebUI_Flags_Library_Table(t *testing.T) {
	u := newUI(t)
	u.write("/films/arrival/movie.mkv", 0, []byte("x"))
	u.write("/films/old/stalker/movie.mkv", 0, []byte("y"))
	u.flush()
	for _, word := range []string{"movies", "tv", "music", "roms", "none"} {
		code, e := u.setFlags("/films", `"library":"`+word+`"`)
		want := word
		if word == "none" {
			want = ""
		}
		if code != 200 || e.Library != want {
			t.Fatalf("set %s: %d %+v", word, code, e)
		}
	}
	for path, body := range map[string]string{"/films/arrival/movie.mkv": `"library":"movies"`, "/": `"library":"movies"`, "/films": `"library":"films"`, "/films/old": `"library":""`} {
		if code, _ := u.setFlags(path, body); code != 400 {
			t.Fatalf("flags %s %s: %d, want 400", path, body, code)
		}
	}
	if code, e := u.setFlags("/films", `"library":"movies","nsfw":true`); code != 200 || e.Library != "movies" || !e.NSFW {
		t.Fatalf("kind beside nsfw: %d %+v", code, e)
	}
	if code, e := u.setFlags("/films/old/stalker", `"library":"tv"`); code != 200 || e.Library != "tv" {
		t.Fatalf("nested library: %d %+v", code, e)
	}
	// effective on the listed folder, own on the entries
	l := u.lsOf("path=/films/old")
	if l.Dir.Library != "movies" || l.entry("stalker").Library != "tv" {
		t.Fatalf("ls /films/old: dir %+v, stalker %+v", l.Dir, l.entry("stalker"))
	}
	if l := u.lsOf("path=/films/old/stalker"); l.Dir.Library != "tv" {
		t.Fatalf("a library inside a library is its own: %+v", l.Dir)
	}
	if root := u.lsOf("path=/"); root.Dir.Library != "" || root.entry("films").Library != "movies" {
		t.Fatalf("root listing: %+v %+v", root.Dir, root.entry("films"))
	}
	if st := u.statOf("/films/arrival/movie.mkv"); st.Library != "movies" {
		t.Fatalf("stat carries the effective kind: %+v", st)
	}
	if code, e := u.setFlags("/films", `"library":"none"`); code != 200 || e.Library != "" || !e.NSFW {
		t.Fatalf("clear the kind, keep nsfw: %d %+v", code, e)
	}
}
