package meta

import (
	"bytes"
	"testing"
)

// SetVolumeMode flips Vol.Auto and nothing else; SetVolume leaves the mode alone.
// Invariant: I-75
func TestFSM_SetVolumeMode(t *testing.T) {
	tf := newTFSM(t)
	if tf.f.Volume().Auto {
		t.Fatal("a fresh volume is manual until init says otherwise")
	}
	tf.ok(Command{Op: OpSetVolumeMode, SetVolumeMode: &SetVolumeMode{Auto: true, Now: 2}})
	if v := tf.f.Volume(); !v.Auto || v.PolicyVersion != 0 || v.K != 2 || v.M != 1 {
		t.Fatalf("after auto: %+v", v)
	}
	tf.ok(Command{Op: OpSetVolume, SetVolume: &SetVolume{K: 3, M: 1, Now: 3}})
	if v := tf.f.Volume(); !v.Auto || v.PolicyVersion != 1 {
		t.Fatalf("SetVolume must not touch the mode: %+v", v)
	}
	tf.ok(Command{Op: OpSetVolumeMode, SetVolumeMode: &SetVolumeMode{Auto: false, Now: 4}})
	if v := tf.f.Volume(); v.Auto || v.PolicyVersion != 1 {
		t.Fatalf("after manual: %+v", v)
	}
}

// A version-1 snapshot (no Auto byte) still decodes, as manual; version 2 round-trips Auto.
// Invariant: I-22
func TestCodec_ReadsVersion1Snapshot(t *testing.T) {
	s := sampleState()
	s.Vol.Auto = false // version 1 has no Auto byte
	var buf bytes.Buffer
	if err := encodeStateVersion(&buf, s, stateVersionV1); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeState(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("version 1: %v", err)
	}
	if got.Vol.Auto || got.Vol.K != s.Vol.K || got.Vol.PolicyVersion != s.Vol.PolicyVersion || len(got.Nodes) != len(s.Nodes) {
		t.Fatalf("version 1 decoded %+v", got.Vol)
	}
	// re-encoding writes the current version
	buf.Reset()
	if err := EncodeState(&buf, got); err != nil {
		t.Fatal(err)
	}
	if buf.Bytes()[4] != stateVersion {
		t.Fatalf("re-encoded version %d", buf.Bytes()[4])
	}
	// a version-2 snapshot (Auto, no Host) reads too
	s.Vol.Auto = true
	buf.Reset()
	if err := encodeStateVersion(&buf, s, stateVersionV2); err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeState(bytes.NewReader(buf.Bytes())); err != nil || !got.Vol.Auto || len(got.Nodes) != len(s.Nodes) {
		t.Fatalf("version 2: %v", err)
	}
}
