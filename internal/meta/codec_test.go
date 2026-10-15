package meta

import (
	"bytes"
	"reflect"
	"testing"
)

// Invariant: I-22, I-156
func TestCodec_StateRoundTrip(t *testing.T) {
	s := sampleState()
	var buf bytes.Buffer
	if err := EncodeState(&buf, s); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("TRM1\x0f")) {
		t.Fatalf("bad header %x", buf.Bytes()[:5])
	}
	got, err := DecodeState(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("round trip mismatch\n got %+v\nwant %+v", got, s)
	}
	// Tombs is rebuilt, not serialized: it must equal the Refs==0 set.
	for h, c := range got.Chunks {
		_, inTombs := got.Tombs[h]
		if (c.Refs == 0) != inTombs {
			t.Fatalf("Tombs inconsistent for %x", h[:2])
		}
	}
}

func TestCodec_EmptyState(t *testing.T) {
	s := NewState()
	var buf bytes.Buffer
	if err := EncodeState(&buf, s); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeState(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("mismatch\n got %+v\nwant %+v", got, s)
	}
}

// Invariant: I-22
func TestCodec_CanonicalOrdering(t *testing.T) {
	a := sampleState()
	// Build an equal state with different insertion order for every map.
	b := NewState()
	b.Vol, b.Meta, b.Applied, b.LastNow, b.NextIno, b.NextNode = a.Vol, a.Meta, a.Applied, a.LastNow, a.NextIno, a.NextNode
	inos := []Ino{4, RootIno, 3, 2}
	for _, i := range inos {
		cp := *a.Inodes[i]
		if cp.Children != nil {
			cp.Children = map[string]Ino{}
			keys := []string{"l", "f", "d"}
			for _, k := range keys {
				if v, ok := a.Inodes[i].Children[k]; ok {
					cp.Children[k] = v
				}
			}
		}
		b.Inodes[i] = &cp
	}
	for _, n := range []NodeIdx{2, 1} {
		cp := *a.Nodes[n]
		b.Nodes[n] = &cp
	}
	for _, hh := range []Hash{sampleHash(2), sampleHash(1)} {
		cp := *a.Chunks[hh]
		b.Chunks[hh] = &cp
		if cp.Refs == 0 {
			b.Tombs[hh] = struct{}{}
		}
	}
	var ba, bb bytes.Buffer
	if err := EncodeState(&ba, a); err != nil {
		t.Fatal(err)
	}
	if err := EncodeState(&bb, b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ba.Bytes(), bb.Bytes()) {
		t.Fatal("equal states encoded differently")
	}
	// Encoding twice is stable.
	var bc bytes.Buffer
	if err := EncodeState(&bc, a); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ba.Bytes(), bc.Bytes()) {
		t.Fatal("encoding not stable")
	}
}

// Invariant: I-22
func TestEncodeState_Canonical(t *testing.T) { TestCodec_CanonicalOrdering(t) }

func TestDecodeState_Rejects(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeState(&buf, sampleState()); err != nil {
		t.Fatal(err)
	}
	good := buf.Bytes()
	bad := map[string][]byte{
		"empty":       {},
		"bad magic":   append([]byte("TRMX"), good[4:]...),
		"bad version": append([]byte("TRM1\x06"), good[5:]...),
		"truncated":   good[:len(good)/2],
		"trailing":    append(append([]byte{}, good...), 0),
	}
	for name, in := range bad {
		if _, err := DecodeState(bytes.NewReader(in)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A HashZero chunk entry or a tombstoned chunk with Refs>0 is invalid state (I-2, I-27).
	s := sampleState()
	s.Chunks[HashZero] = &Chunk{Size: 1, K: 1, M: 0, Placement: []NodeIdx{1}}
	buf.Reset()
	if err := EncodeState(&buf, s); err == nil {
		if _, err := DecodeState(bytes.NewReader(buf.Bytes())); err == nil {
			t.Error("HashZero chunk entry accepted")
		}
	}
}

// Invariant: I-22
func FuzzDecodeState(f *testing.F) {
	for _, s := range []*State{NewState(), sampleState()} {
		var buf bytes.Buffer
		if err := EncodeState(&buf, s); err != nil {
			f.Fatal(err)
		}
		f.Add(buf.Bytes())
	}
	f.Add([]byte("TRM1"))
	f.Fuzz(func(t *testing.T, in []byte) {
		s, err := DecodeState(bytes.NewReader(in))
		if err != nil {
			return
		}
		var out bytes.Buffer
		if err := EncodeState(&out, s); err != nil {
			t.Fatalf("re-encode of decoded state failed: %v", err)
		}
		if !bytes.Equal(out.Bytes(), in) {
			t.Fatalf("decode/encode not identity")
		}
	})
}

// withoutRewrites is s as a snapshot older than version 9 holds it: no inode records a
// rewrite (docs/04 §6, I-223).
func withoutRewrites(s *State) *State {
	for _, in := range s.Inodes {
		in.RewriteAt = 0
	}
	return s
}

// A version 7 snapshot, written before Inode.RewriteAt, keeps its snapshot catalogue
// and decodes with no rewrite recorded: the catalogue is gated on version 7, not on
// the newest version (a gate on stateVersion would have dropped it at version 9).
// Invariant: I-223, I-184
func TestCodec_ReadsVersion7Snapshot(t *testing.T) {
	s := sampleState()
	s.Snaps = map[SnapID]*Snapshot{3: {ID: 3, Mirror: 2, Name: "daily", Index: 6, At: 50, Auto: true, State: SnapComplete, Root: sampleHash(9), Files: 4, Bytes: 100, NewBytes: 10}}
	s.NextSnap = 4
	var buf bytes.Buffer
	if err := encodeStateVersion(&buf, s, stateVersionV7); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeState(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if want := withoutRewrites(s.Clone()); !reflect.DeepEqual(got, want) {
		t.Fatalf("v7 round trip\n got %+v\nwant %+v", got, want)
	}
	// and version 9 keeps both
	buf.Reset()
	s = sampleState()
	s.Snaps, s.NextSnap = map[SnapID]*Snapshot{3: {ID: 3, Mirror: 2, Name: "daily", State: SnapComplete}}, 4
	if err := EncodeState(&buf, s); err != nil {
		t.Fatal(err)
	}
	if got, err = DecodeState(bytes.NewReader(buf.Bytes())); err != nil || !reflect.DeepEqual(got, s) {
		t.Fatalf("v9 round trip: err=%v\n got %+v\nwant %+v", err, got, s)
	}
}
