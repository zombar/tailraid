package meta

import (
	"bytes"
	"reflect"
	"testing"
)

func init() {
	goldenCommands = append(goldenCommands, struct {
		name string
		cmd  Command
		hex  string
	}{
		"Playback", // codec version 17 (docs/13 §10c, I-406)
		Command{Op: OpPlayback, Playback: &Playback{Viewer: "u:1", Action: PlayPosition, Inos: []Ino{5}, Pos: 1000, Dur: 2000, On: "n", Now: 9}},
		"11 22 03753a31 01 01 0500000000000000 e803000000000000 d007000000000000 016e 0900000000000000",
	})
}

func pos(viewer string, ino Ino, p, d uint64) Command {
	return Command{Op: OpPlayback, Playback: &Playback{Viewer: viewer, Action: PlayPosition, Inos: []Ino{ino}, Pos: p, Dur: d, On: "pike", Now: 50}}
}

func act(viewer string, a PlayAction, inos ...Ino) Command {
	return Command{Op: OpPlayback, Playback: &Playback{Viewer: viewer, Action: a, Inos: inos, On: "pike", Now: 50}}
}

func (tf *tfsm) prow(viewer string, ino Ino) PlayRow {
	tf.t.Helper()
	r, ok := tf.f.PlaybackRow(viewer, ino)
	if !ok {
		tf.t.Fatalf("no row of %s for %d", viewer, ino)
	}
	return r
}

// Playback is codec 17 and only it: it round-trips, carries a viewer as a plain command,
// and the same body in any older version, or another op in 17, is refused.
// Invariant: I-406
func TestEncodeCommand_V17Playback(t *testing.T) {
	if CodecPlayback != commandVersionV17 || CodecVersion < commandVersionV17 {
		t.Fatalf("CodecPlayback %d, CodecVersion %d", CodecPlayback, CodecVersion)
	}
	c := act("t:pat@example.com", PlayWatched, 3, 4, 5)
	if v := CommandCodec(c); v != commandVersionV17 {
		t.Fatalf("version %d", v)
	}
	b := MustEncodeCommand(c)
	dec, err := DecodeCommand(b)
	if err != nil || !reflect.DeepEqual(dec, c) {
		t.Fatalf("round trip: %v\n got %+v\nwant %+v", err, dec.Playback, c.Playback)
	}
	for _, v := range []uint8{commandVersion, commandVersionV11, commandVersionV15, commandVersionV16} {
		old := append([]byte(nil), b...)
		old[0] = v
		if _, err := DecodeCommand(old); err == nil {
			t.Errorf("a playback decoded in version %d", v)
		}
	}
	mk := MustEncodeCommand(Command{Op: OpMkdir, Mkdir: &Mkdir{Parent: 1, Name: "d", Now: 1}})
	mk[0] = commandVersionV17
	if _, err := DecodeCommand(mk); err == nil {
		t.Error("a mkdir decoded in version 17")
	}
	// a batch past MaxPlayBatch is malformed on the wire
	big := act("u:1", PlayWatched)
	for i := range MaxPlayBatch + 1 {
		big.Playback.Inos = append(big.Playback.Inos, Ino(i+2))
	}
	if _, err := DecodeCommand(MustEncodeCommand(big)); err == nil {
		t.Error("a batch past MaxPlayBatch decoded")
	}
}

// The apply rules of docs/02 §Apply rules: shapes checked before anything changes, the
// position clamped, watched kept, plays counted, hidden cleared by the next position.
// Invariant: I-405
func TestFSM_Playback_Table(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "film.mkv")
	g := tf.create(RootIno, "other.mkv")
	d := tf.mkdir(RootIno, "dir")
	tf.fails(pos("x", f, 1, 2), ErrBadCommand)  // not a viewer key
	tf.fails(pos("u:", f, 1, 2), ErrBadCommand) // too short
	tf.fails(pos("u:"+string(bytes.Repeat([]byte("a"), ViewerMax)), f, 1, 2), ErrBadCommand)
	tf.fails(pos("u:1", f, 1, 0), ErrBadCommand)           // no duration
	tf.fails(pos("u:1", d, 1, 2), ErrNotExist)             // a directory
	tf.fails(pos("u:1", 999, 1, 2), ErrNotExist)           // no such inode
	tf.fails(act("u:1", PlayHide, f), ErrNotExist)         // nothing to hide
	tf.fails(act("u:1", PlayAction(9), f), ErrBadCommand)  // unknown action
	tf.fails(act("u:1", PlayPosition), ErrBadCommand)      // no inode
	tf.fails(act("u:1", PlayWatched), ErrBadCommand)       // an empty batch
	tf.fails(act("u:1", PlayWatched, f, 999), ErrNotExist) // one bad inode refuses the batch
	long := pos("u:1", f, 1, 2)
	long.Playback.On = string(bytes.Repeat([]byte("n"), PlayOnMax+1))
	tf.fails(long, ErrBadCommand)

	tf.ok(pos("u:1", f, 5000, 3000)) // clamped
	if r := tf.prow("u:1", f); r.Pos != 3000 || r.Dur != 3000 || !r.Watched || r.On != "pike" {
		t.Fatalf("clamped: %+v", r)
	}
	tf.ok(pos("u:1", f, 100, 3000)) // a rewatch keeps watched
	if r := tf.prow("u:1", f); r.Pos != 100 || !r.Watched {
		t.Fatalf("rewatch: %+v", r)
	}
	tf.ok(act("u:1", PlayUnwatched, f))
	if r := tf.prow("u:1", f); r.Pos != 0 || r.Watched {
		t.Fatalf("unwatched: %+v", r)
	}
	tf.ok(act("u:1", PlayHide, f))
	if !tf.prow("u:1", f).Hidden {
		t.Fatal("not hidden")
	}
	tf.ok(pos("u:1", f, 10, 3000))
	if tf.prow("u:1", f).Hidden {
		t.Fatal("a position kept it hidden")
	}
	tf.ok(act("u:1", PlayPlayed, g))
	tf.ok(act("u:1", PlayPlayed, g))
	if r := tf.prow("u:1", g); r.Plays != 2 || r.Dur != 0 {
		t.Fatalf("plays: %+v", r)
	}
	tf.ok(act("t:pat@example.com", PlayWatched, f, g))
	if !tf.prow("t:pat@example.com", f).Watched || !tf.prow("t:pat@example.com", g).Watched {
		t.Fatal("batch watched")
	}
	if tf.prow("u:1", g).Watched {
		t.Fatal("one viewer's batch reached another")
	}
	// a file in the trash takes no report, and keeps its rows
	tf.trash(RootIno, "other.mkv", false)
	tf.fails(act("u:1", PlayPlayed, g), ErrNotExist)
	if tf.prow("u:1", g).Plays != 2 {
		t.Fatal("a trashed file lost its row")
	}
}

// Watched is 90 % of the duration, or the last five minutes of anything over thirty, in
// integer milliseconds.
// Invariant: I-405
func TestFSM_Playback_WatchedRule_Table(t *testing.T) {
	const minute = 60 * 1000
	for _, c := range []struct {
		pos, dur uint64
		want     bool
	}{
		{0, 0, false},
		{0, 1, false},
		{1, 1, true},
		{8999, 10000, false},
		{9000, 10000, true},
		{13, 15, false},
		{14, 15, true},
		{26 * minute, 30 * minute, false},
		{27 * minute, 30 * minute, true},     // 90 %
		{25*minute + 1, 30*minute + 1, true}, // over thirty: the last five minutes
		{25*minute - 1, 30*minute + 1, false},
		{115 * minute, 120 * minute, true}, // a film: 90 % is 108 minute
		{108*minute - 1, 120 * minute, false},
		{108 * minute, 120 * minute, true},
	} {
		if got := WatchedAt(c.pos, c.dur); got != c.want {
			t.Errorf("WatchedAt(%d, %d) = %v, want %v", c.pos, c.dur, got, c.want)
		}
	}
}

// A viewer keeps at most PlaybackRows rows, the oldest (At, Ino) dropped; at most
// PlaybackViewers viewers, a new one past it ErrPlaybackFull.
// Invariant: I-405
func TestFSM_Playback_Bounds(t *testing.T) {
	tf := newTFSM(t)
	var files []Ino
	for i := range 3 {
		files = append(files, tf.create(RootIno, string(rune('a'+i))))
	}
	st := tf.f.state()
	// fill the viewer to the bound with rows of inodes that are not files, as a long
	// history would, older than anything the test reports
	rows := map[Ino]*PlayRow{}
	for i := range PlaybackRows {
		rows[Ino(1_000_000+i)] = &PlayRow{Dur: 1, At: int64(i % 7)}
	}
	st.Playback["u:1"] = rows
	tf.f.stats.play = buildPlayIndex(st)
	tf.ok(pos("u:1", files[0], 1, 10))
	if n := len(tf.f.PlaybackRows("u:1")); n != PlaybackRows {
		t.Fatalf("%d rows, want %d", n, PlaybackRows)
	}
	if _, ok := tf.f.PlaybackRow("u:1", 1_000_000); ok {
		t.Fatal("the oldest (At 0, lowest inode) row stayed")
	}
	if _, ok := tf.f.PlaybackRow("u:1", 1_000_007); !ok {
		t.Fatal("a row of At 0 above the lowest inode went")
	}
	if _, ok := tf.f.PlaybackRow("u:1", files[0]); !ok {
		t.Fatal("the new row went")
	}
	for i := range PlaybackViewers - 1 {
		st.Playback["u:x"+string(rune('A'+i%26))+string(rune('A'+i/26))] = map[Ino]*PlayRow{files[1]: {Dur: 1}}
	}
	tf.f.stats.play = buildPlayIndex(st)
	tf.fails(pos("u:new", files[2], 1, 10), ErrPlaybackFull)
	tf.ok(pos("u:1", files[2], 1, 10)) // an existing viewer still reports
}

// A file deleted for good takes its rows from every viewer, and a viewer left with none
// goes; a trashed file keeps them until its purge.
// Invariant: I-405
func TestFSM_Playback_PurgeCascade(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	g := tf.create(RootIno, "g")
	tf.ok(pos("u:1", f, 1, 10))
	tf.ok(pos("u:1", g, 1, 10))
	tf.ok(pos("u:2", f, 1, 10))
	tf.trash(RootIno, "f", false)
	if _, ok := tf.f.PlaybackRow("u:2", f); !ok {
		t.Fatal("a trashed file lost its row")
	}
	tf.ok(Command{Op: OpPurge, Purge: &Purge{Ino: f, Now: 60}})
	if _, ok := tf.f.PlaybackRow("u:1", f); ok {
		t.Fatal("u:1 kept a purged file's row")
	}
	if _, ok := tf.f.PlaybackRow("u:1", g); !ok {
		t.Fatal("u:1 lost another file's row")
	}
	if _, ok := tf.f.state().Playback["u:2"]; ok {
		t.Fatal("a viewer with no rows left stayed")
	}
	// an unlink deletes for good too
	tf.ok(Command{Op: OpUnlink, Unlink: &Unlink{Parent: RootIno, Name: "g", Now: 70}})
	if len(tf.f.state().Playback) != 0 {
		t.Fatalf("rows left: %+v", tf.f.state().Playback)
	}
}

// Frame 16 round-trips the rows byte-identically and a frame 15 snapshot restores none;
// the per-inode index is rebuilt on Restore.
// Invariant: I-406, I-405
func TestState_Frame16_RoundTrip(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	tf.ok(pos("u:1", f, 5, 10))
	tf.ok(act("t:pat@example.com", PlayPlayed, f))
	a := encodeState(t, tf.f.state())
	if a[4] != stateVersion { // frame 16 added the rows; the newest frame still carries them
		t.Fatalf("frame %d", a[4])
	}
	st, err := DecodeState(bytes.NewReader(a))
	if err != nil {
		t.Fatal(err)
	}
	if b := encodeState(t, st); !bytes.Equal(a, b) {
		t.Fatal("frame 16 does not round-trip byte-identically")
	}
	if got := st.Playback["u:1"][f]; got == nil || got.Pos != 5 || got.Dur != 10 || got.On != "pike" {
		t.Fatalf("decoded row %+v", got)
	}
	if v := playIndexDrift(buildPlayIndex(tf.f.state()), buildPlayIndex(st)); v != nil {
		t.Fatal(v)
	}
	var old bytes.Buffer
	if err := encodeStateVersion(&old, tf.f.state(), stateVersionV15); err != nil {
		t.Fatal(err)
	}
	st15, err := DecodeState(bytes.NewReader(old.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(st15.Playback) != 0 || st15.Playback == nil {
		t.Fatalf("frame 15 restored %v", st15.Playback)
	}
}
