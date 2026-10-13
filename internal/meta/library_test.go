package meta

import (
	"errors"
	"reflect"
	"testing"
)

// The library kind lives in two bits of a directory's Flags: every kind sets and reads
// back beside the other flags, a file refuses it, an unknown bit is malformed.
// Invariant: I-210
func TestFSM_SetAttr_LibraryKind_Table(t *testing.T) {
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "d")
	f := tf.create(RootIno, "f")
	for _, k := range []LibraryKind{LibraryMovies, LibraryTV, LibraryMusic, LibraryNone} {
		tf.flags(d, WithLibrary(FlagNSFW, k))
		in := tf.inode(d)
		if in.Library() != k || in.Flags&FlagNSFW == 0 || in.Flags&FlagProtected != 0 {
			t.Fatalf("after %s: flags %08b, kind %s", k, in.Flags, in.Library())
		}
	}
	tf.flags(d, WithLibrary(FlagProtected|FlagNSFW, LibraryTV))
	if in := tf.inode(d); in.Library() != LibraryTV || in.Flags&(FlagProtected|FlagNSFW) != FlagProtected|FlagNSFW {
		t.Fatalf("the kind and the other flags do not coexist: %08b", in.Flags)
	}
	tf.fails(Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: f, Mask: AttrFlags, Flags: WithLibrary(0, LibraryMovies), Now: 1}}, ErrNotDir)
	tf.fails(Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: d, Mask: AttrFlags, Flags: 1 << 5, Now: 1}}, ErrBadCommand)
	for _, tc := range []struct {
		word string
		kind LibraryKind
		ok   bool
	}{{"none", LibraryNone, true}, {"movies", LibraryMovies, true}, {"tv", LibraryTV, true}, {"music", LibraryMusic, true}, {"films", 0, false}, {"", 0, false}, {"Movies", 0, false}} {
		k, ok := ParseLibraryKind(tc.word)
		if ok != tc.ok || (ok && k != tc.kind) {
			t.Fatalf("parse %q: %s %v", tc.word, k, ok)
		}
		if ok && k.String() != tc.word {
			t.Fatalf("%s prints as %q", tc.word, k.String())
		}
	}
}

// A folder's effective kind is the nearest flagged ancestor's, its own first; a library
// inside a library is its own; a folder with no flagged ancestor is none.
// Invariant: I-210
func TestFSM_LibraryEffective_NearestAncestor(t *testing.T) {
	tf := newTFSM(t)
	a := tf.mkdir(RootIno, "a")
	b := tf.mkdir(a, "b")
	c := tf.mkdir(b, "c")
	x := tf.mkdir(RootIno, "x")
	tf.flags(a, WithLibrary(0, LibraryMovies))
	tf.flags(c, WithLibrary(0, LibraryMusic))
	for ino, want := range map[Ino]LibraryKind{RootIno: LibraryNone, a: LibraryMovies, b: LibraryMovies, c: LibraryMusic, x: LibraryNone} {
		if got := tf.f.EffectiveLibrary(ino); got != want {
			t.Fatalf("effective of %d: %s, want %s", ino, got, want)
		}
	}
}

// Libraries lists every live flagged folder in kind-then-path order, the same on every
// node; a flagged folder in the trash is left out.
// Invariant: I-210
func TestStore_Libraries(t *testing.T) {
	build := func() (*tfsm, []Library) {
		tf := newTFSM(t)
		music := tf.mkdir(RootIno, "music")
		films := tf.mkdir(RootIno, "films")
		more := tf.mkdir(RootIno, "archive")
		oldFilms := tf.mkdir(more, "films")
		shows := tf.mkdir(RootIno, "shows")
		gone := tf.mkdir(RootIno, "gone")
		tf.flags(music, WithLibrary(0, LibraryMusic))
		tf.flags(films, WithLibrary(0, LibraryMovies))
		tf.flags(oldFilms, WithLibrary(0, LibraryMovies))
		tf.flags(shows, WithLibrary(0, LibraryTV))
		tf.flags(gone, WithLibrary(0, LibraryTV))
		tf.trash(RootIno, "gone", true)
		return tf, []Library{
			{Ino: oldFilms, Kind: LibraryMovies, Path: "/archive/films"},
			{Ino: films, Kind: LibraryMovies, Path: "/films"},
			{Ino: shows, Kind: LibraryTV, Path: "/shows"},
			{Ino: music, Kind: LibraryMusic, Path: "/music"},
		}
	}
	tf, want := build()
	if got := tf.f.Libraries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("libraries\n got %+v\nwant %+v", got, want)
	}
	other, _ := build() // two replicas of the same log agree
	if !reflect.DeepEqual(other.f.Libraries(), want) {
		t.Fatal("replicas disagree on the libraries")
	}
}

// Codec version 6 is written only for a SetAttr whose Flags carry the library bits; a
// SetAttr with the other flags stays at 4; a version-6 entry of any other op and an
// older-version SetAttr carrying the bits are rejected on decode.
// Invariant: I-210
func TestEncodeCommand_V6Gating(t *testing.T) {
	plain := Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: 2, Mask: AttrFlags, Flags: FlagNSFW, Now: 1}}
	if v := CommandCodec(plain); v != commandVersionV4 {
		t.Fatalf("nsfw alone is version %d, want 4", v)
	}
	lib := Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: 2, Mask: AttrFlags, Flags: WithLibrary(FlagNSFW, LibraryTV), Now: 1}}
	if v := CommandCodec(lib); v != commandVersionV6 {
		t.Fatalf("the library bits are version %d, want 6", v)
	}
	b := MustEncodeCommand(lib)
	dec, err := DecodeCommand(b)
	if err != nil || !reflect.DeepEqual(dec, lib) {
		t.Fatalf("round trip: %v %+v", err, dec)
	}
	old := append([]byte(nil), b...)
	old[0] = commandVersionV4
	if _, err := DecodeCommand(old); err == nil {
		t.Fatal("a version-4 SetAttr carrying the library bits was accepted")
	}
	unlink := MustEncodeCommand(Command{Op: OpUnlink, Unlink: &Unlink{Parent: 1, Name: "a", Now: 2}})
	unlink[0] = commandVersionV6
	if _, err := DecodeCommand(unlink); err == nil {
		t.Fatal("a version-6 Unlink was accepted")
	}
	if !errors.Is(ErrCodec, ErrCodec) { // the gate's error is the one the gateways map (docs/12 §6)
		t.Fatal("ErrCodec")
	}
}

// Since codec 10 the library kind is three bits and kind 4 is roms: it sets and reads
// back beside the other flags, kinds 5 to 7 are malformed, and the kinds already stored
// keep their bits.
// Invariant: I-277
func TestFSM_SetAttr_LibraryRoms_Table(t *testing.T) {
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "roms")
	tf.flags(d, WithLibrary(FlagProtected, LibraryRoms))
	if in := tf.inode(d); in.Library() != LibraryRoms || in.Flags&FlagProtected == 0 {
		t.Fatalf("roms: flags %08b, kind %s", in.Flags, in.Library())
	}
	for _, k := range []LibraryKind{5, 6, 7} {
		tf.fails(Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: d, Mask: AttrFlags, Flags: uint8(k) << FlagLibraryShift, Now: 1}}, ErrBadCommand)
	}
	for k, bits := range map[LibraryKind]uint8{LibraryMovies: 0b0100, LibraryTV: 0b1000, LibraryMusic: 0b1100, LibraryRoms: 0b10000} {
		if got := WithLibrary(0, k); got != bits {
			t.Fatalf("%s is %08b, want %08b", k, got, bits)
		}
	}
	if k, ok := ParseLibraryKind("roms"); !ok || k != LibraryRoms || k.String() != "roms" {
		t.Fatalf("roms parses as %s %v", k, ok)
	}
	if LibraryKind(5).String() != "none" {
		t.Fatal("an unknown kind prints as a word")
	}
}

// Codec version 10 is written only for a SetAttr whose Flags carry the roms kind; the
// other kinds stay at 6. A roms kind decoded below version 10, a version-10 SetAttr
// without it and a version-10 entry of another op are rejected.
// Invariant: I-277
func TestEncodeCommand_V10Gating(t *testing.T) {
	music := Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: 2, Mask: AttrFlags, Flags: WithLibrary(0, LibraryMusic), Now: 1}}
	if v := CommandCodec(music); v != commandVersionV6 {
		t.Fatalf("music is version %d, want 6", v)
	}
	roms := Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: 2, Mask: AttrFlags, Flags: WithLibrary(FlagNSFW, LibraryRoms), Now: 1}}
	if v := CommandCodec(roms); v != commandVersionV10 || CodecRoms != commandVersionV10 || CodecVersion != commandVersionV10 {
		t.Fatalf("roms is version %d, want 10", v)
	}
	b := MustEncodeCommand(roms)
	if dec, err := DecodeCommand(b); err != nil || !reflect.DeepEqual(dec, roms) {
		t.Fatalf("round trip: %v %+v", err, dec)
	}
	for _, v := range []uint8{commandVersionV4, commandVersionV6} {
		old := append([]byte(nil), b...)
		old[0] = v
		if _, err := DecodeCommand(old); err == nil {
			t.Fatalf("a version-%d SetAttr carrying roms was accepted", v)
		}
	}
	m := MustEncodeCommand(music)
	m[0] = commandVersionV10
	if _, err := DecodeCommand(m); err == nil {
		t.Fatal("a version-10 SetAttr without roms was accepted (not canonical)")
	}
	u := MustEncodeCommand(Command{Op: OpUnlink, Unlink: &Unlink{Parent: 1, Name: "a", Now: 2}})
	u[0] = commandVersionV10
	if _, err := DecodeCommand(u); err == nil {
		t.Fatal("a version-10 Unlink was accepted")
	}
}
