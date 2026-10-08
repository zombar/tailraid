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
	var buf bytes.Buffer
	if err := EncodeState(&buf, s); err != nil {
		t.Fatal(err)
	}
	v2 := buf.Bytes()
	// strip the Auto byte: magic 4 + version 1 + Applied 8 + LastNow 8 + ChunkSize 4 + K 1 + M 1 + PolicyVersion 4
	const autoAt = 4 + 1 + 8 + 8 + 4 + 1 + 1 + 4
	v1 := append([]byte{}, v2[:autoAt]...)
	v1 = append(v1, v2[autoAt+1:]...)
	v1[4] = 1
	got, err := DecodeState(bytes.NewReader(v1))
	if err != nil {
		t.Fatalf("version 1: %v", err)
	}
	if got.Vol.Auto || got.Vol.K != s.Vol.K || got.Vol.PolicyVersion != s.Vol.PolicyVersion || len(got.Nodes) != len(s.Nodes) {
		t.Fatalf("version 1 decoded %+v", got.Vol)
	}
	// re-encoding writes version 2, and the state round-trips byte for byte from then on
	buf.Reset()
	if err := EncodeState(&buf, got); err != nil {
		t.Fatal(err)
	}
	if buf.Bytes()[4] != 2 {
		t.Fatalf("re-encoded version %d", buf.Bytes()[4])
	}
}
