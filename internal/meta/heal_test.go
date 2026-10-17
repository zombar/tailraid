package meta

import (
	"reflect"
	"testing"
)

func init() {
	goldenCommands = append(goldenCommands, struct {
		name string
		cmd  Command
		hex  string
	}{
		"SnapshotHeal", // codec version 15 (docs/16 §5, I-393)
		Command{Op: OpSnapshotHeal, SnapshotHeal: &SnapshotHeal{ID: 42, Missing: 1, Now: 9}},
		"0f 21 2a000000 01000000 0900000000000000",
	})
}

// A heal is codec 15 and only it: it round-trips, alone and under a codec-14 header whose
// inner body keeps version 15 (the leader's gate then asks 15 of every member), and the
// same body in any older version is refused.
// Invariant: I-393
func TestCodec_SnapshotHeal_Version15(t *testing.T) {
	if CodecHeal != commandVersionV15 || CodecVersion != commandVersionV15 {
		t.Fatalf("CodecHeal %d, CodecVersion %d", CodecHeal, CodecVersion)
	}
	plain := Command{Op: OpSnapshotHeal, SnapshotHeal: &SnapshotHeal{ID: 7, Missing: 0, Now: 3}}
	sys := Command{Op: OpSnapshotHeal, System: true, SnapshotHeal: &SnapshotHeal{ID: 7, Missing: 2, Now: 4}}
	for _, c := range []Command{plain, sys} {
		if v := CommandCodec(c); v != commandVersionV15 {
			t.Fatalf("%+v: version %d", c, v)
		}
		b := MustEncodeCommand(c)
		dec, err := DecodeCommand(b)
		if err != nil || !reflect.DeepEqual(normalizeCommand(dec), normalizeCommand(c)) {
			t.Fatalf("round trip: %v\n got %+v\nwant %+v", err, dec, c)
		}
	}
	b := MustEncodeCommand(sys)
	if b[0] != commandVersionV14 || b[4] != commandVersionV15 {
		t.Fatalf("a heal as System: %x, want a codec-14 header around a version-15 body", b[:6])
	}
	pb := MustEncodeCommand(plain)
	if pb[0] != commandVersionV15 {
		t.Fatalf("a plain heal is version %d", pb[0])
	}
	for _, v := range []uint8{commandVersion, commandVersionV7, commandVersionV11, commandVersionV14} {
		old := append([]byte(nil), pb...)
		old[0] = v
		if _, err := DecodeCommand(old); err == nil {
			t.Errorf("a heal decoded in version %d", v)
		}
	}
	// version 15 carries nothing but the heal
	mk := MustEncodeCommand(Command{Op: OpMkdir, Mkdir: &Mkdir{Parent: 1, Name: "d", Now: 1}})
	mk[0] = commandVersionV15
	if _, err := DecodeCommand(mk); err == nil {
		t.Error("a mkdir decoded in version 15")
	}
}

// A heal lowers a complete or incomplete row's missing count and makes it complete at
// zero; it never raises the count, and a syncing, deleting or unknown row is ErrNotExist
// (docs/02 §SnapshotHeal, docs/16 §5).
// Invariant: I-391
func TestFSM_SnapshotHeal_Table(t *testing.T) {
	tf, m := mirrorFSM(t)
	heal := func(id SnapID, missing uint32) Command {
		return Command{Op: OpSnapshotHeal, SnapshotHeal: &SnapshotHeal{ID: id, Missing: missing, Now: 100}}
	}
	id := tf.begin(m, "nightly", 10)
	tf.fails(heal(id, 0), ErrNotExist) // still syncing
	tf.ok(Command{Op: OpSnapshotEnd, SnapshotEnd: &SnapshotEnd{ID: id, OK: true, Missing: 3, Now: 20}})
	tf.fails(heal(id, 4), ErrBadCommand) // the count only falls
	tf.fails(heal(99, 0), ErrNotExist)
	tf.ok(heal(id, 3)) // no change, no harm
	if r := tf.row(id); r.State != SnapIncomplete || r.Missing != 3 {
		t.Fatalf("after an equal heal: %+v", r)
	}
	tf.ok(heal(id, 1))
	if r := tf.row(id); r.State != SnapIncomplete || r.Missing != 1 {
		t.Fatalf("after healing two: %+v", r)
	}
	tf.ok(heal(id, 0))
	r := tf.row(id)
	if r.State != SnapComplete || r.Missing != 0 || r.At != 10 || r.Name != "nightly" {
		t.Fatalf("after healing the last: %+v", r)
	}
	tf.ok(heal(id, 0)) // a repeated heal of a complete row changes nothing
	tf.fails(heal(id, 1), ErrBadCommand)
	tf.ok(Command{Op: OpSnapshotDelete, SnapshotDelete: &SnapshotDelete{ID: id, Now: 30}})
	tf.fails(heal(id, 0), ErrNotExist) // deleting
}
