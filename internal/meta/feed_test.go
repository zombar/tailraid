package meta

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/testseed"
)

// records is every inode's record, printed (maps print sorted), for a before/after diff.
func records(st *State) map[Ino]string {
	out := make(map[Ino]string, len(st.Inodes))
	for ino, in := range st.Inodes {
		out[ino] = fmt.Sprintf("%+v", *in)
	}
	return out
}

// Every inode an entry created, changed or deleted (its record differs, appears or is
// gone, a parent's name table included) is in the feed for that entry, over random
// sequences of every inode command (I-386).
// Invariant: I-386
func TestFSM_Changes_ListEveryChangedInode(t *testing.T) {
	rng := testseed.Seed(t)
	f := NewFSM()
	gen := &cmdGen{rng: rng, f: f, now: 1000}
	idx := uint64(1)
	f.Apply(&raft.Log{Index: idx, Type: raft.LogCommand, Data: MustEncodeCommand(Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: testCS, K: 2, M: 1, Now: 1}})})
	changedSteps := 0
	for step := range 4000 {
		c := gen.next()
		before := records(f.state())
		idx++
		f.Apply(&raft.Log{Index: idx, Type: raft.LogCommand, Data: MustEncodeCommand(c)})
		after := records(f.state())
		got, upto, ok := f.Changes(idx - 1)
		if !ok || upto != idx {
			t.Fatalf("step %d: Changes(%d) = upto %d, complete %v", step, idx-1, upto, ok)
		}
		var missed []Ino
		for ino, r := range after {
			if before[ino] != r && !slices.Contains(got, ino) {
				missed = append(missed, ino)
			}
		}
		for ino := range before {
			if _, ok := after[ino]; !ok && !slices.Contains(got, ino) {
				missed = append(missed, ino)
			}
		}
		if len(missed) > 0 {
			t.Fatalf("step %d op %d: inodes %v changed but not in the feed %v", step, c.Op, missed, got)
		}
		if len(got) > 0 {
			changedSteps++
		}
	}
	if changedSteps < 1000 {
		t.Fatalf("only %d steps changed an inode", changedSteps)
	}
	// from the start, the whole run is one answer, each inode once
	all, upto, ok := f.Changes(0)
	if !ok || upto != idx || !slices.IsSorted(all) || len(slices.Compact(slices.Clone(all))) != len(all) {
		t.Fatalf("Changes(0): %d inodes, upto %d, complete %v", len(all), upto, ok)
	}
}

// The feed answers from its floor on: a restored FSM cannot say what changed before the
// snapshot, and a feed past its bound drops its oldest half and raises the floor (I-386).
// Invariant: I-386
func TestFSM_Changes_FloorAndOverflow(t *testing.T) {
	tf := newTFSM(t)
	a := tf.create(RootIno, "a")
	at := tf.idx
	b := tf.create(RootIno, "b")
	if got, _, ok := tf.f.Changes(at); !ok || !slices.Equal(got, []Ino{RootIno, b}) {
		t.Fatalf("since a's create: %v %v", got, ok)
	}
	if got, upto, ok := tf.f.Changes(tf.idx); !ok || len(got) != 0 || upto != tf.idx {
		t.Fatalf("nothing since now: %v %d %v", got, upto, ok)
	}
	if _, _, ok := tf.f.Changes(tf.idx + 1); ok {
		t.Fatal("an index past the applied one answered")
	}
	// a restore: complete only from the restored index
	sn, err := tf.f.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	r := NewFSM()
	restoreFrom(t, r, sn)
	if _, _, ok := r.Changes(0); ok {
		t.Fatal("a restored FSM answered for the time before its snapshot")
	}
	if got, upto, ok := r.Changes(tf.idx); !ok || len(got) != 0 || upto != tf.idx {
		t.Fatalf("restored, from its index: %v %d %v", got, upto, ok)
	}
	// overflow: a feed of 8 drops its oldest half
	fd := changeFeed{max: 8}
	for i := range 9 {
		fd.add(uint64(10+i), Ino(100+i))
	}
	if fd.floor != 13 || len(fd.log) != 5 {
		t.Fatalf("after overflow: floor %d, %d records", fd.floor, len(fd.log))
	}
	if _, ok := fd.since(12); ok {
		t.Fatal("answered from below the floor")
	}
	if got, ok := fd.since(13); !ok || !slices.Equal(got, []Ino{104, 105, 106, 107, 108}) {
		t.Fatalf("since the floor: %v %v", got, ok)
	}
	_ = a
}

// restoreFrom installs snapshot sn into f, as Raft does on a follower.
func restoreFrom(t *testing.T, f *FSM, sn raft.FSMSnapshot) {
	t.Helper()
	var buf bytes.Buffer
	if err := EncodeState(&buf, sn.(*snapshot).st); err != nil {
		t.Fatal(err)
	}
	if err := f.Restore(io.NopCloser(&buf)); err != nil {
		t.Fatal(err)
	}
}

// The rare paths, deterministically: a folder trashed after a file inside it folds the
// file's root back in, and a restore and a purge name what they moved or removed (I-386).
// Invariant: I-386
func TestFSM_Changes_FoldRestorePurge(t *testing.T) {
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "d")
	f := tf.create(d, "f")
	tf.trash(d, "f", false)
	at := tf.idx
	tf.trash(RootIno, "d", true) // folds f back under d
	if got, _, _ := tf.f.Changes(at); !slices.Contains(got, f) || !slices.Contains(got, d) {
		t.Fatalf("the fold: %v, want %d and %d", got, f, d)
	}
	at = tf.idx
	tf.ok(Command{Op: OpRestore, Restore: &Restore{Ino: d, Now: 2}})
	if got, _, _ := tf.f.Changes(at); !slices.Equal(got, []Ino{RootIno, d}) {
		t.Fatalf("the restore: %v", got)
	}
	tf.trash(RootIno, "d", true)
	at = tf.idx
	tf.ok(Command{Op: OpPurge, Purge: &Purge{Ino: d, Now: 3}})
	if got, _, _ := tf.f.Changes(at); !slices.Contains(got, f) || !slices.Contains(got, d) {
		t.Fatalf("the purge: %v", got)
	}
}
