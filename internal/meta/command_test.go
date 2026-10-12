package meta

import (
	"bytes"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

func h(s string) []byte {
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		panic(err)
	}
	return b
}

func repeatHex(b byte, n int) string { return strings.Repeat(hex.EncodeToString([]byte{b}), n) }

// goldenCommands pins the wire format of every Op. Changing any of these bytes is a
// codec version bump, never a silent change.
var goldenCommands = []struct {
	name string
	cmd  Command
	hex  string
}{
	{
		"SetVolume",
		Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: 1 << 20, K: 2, M: 1, Now: 7}},
		"01 01 00001000 02 01 0700000000000000",
	},
	{
		"MetaSet",
		Command{Op: OpMetaSet, MetaSet: &MetaSet{ClusterID: [16]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, JoinToken: "abcdefgh", CreatedAt: 1}},
		"01 02 000102030405060708090a0b0c0d0e0f 08 6162636465666768 0100000000000000",
	},
	{
		"NodeUpsert",
		Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{
			ID: "0123456789abcdef", Name: "n", Kind: KindDevice, State: Active,
			TSStableID: "27", Addr: "100.64.0.11", RaftPort: 20480, RPCPort: 20481, Voter: true, Capacity: 1024, Version: "v1", JoinedAt: 13,
		}}},
		"01 03 0000 10 30313233343536373839616263646566 01 6e 01 01 02 3237 0b 3130302e36342e302e3131 0050 0150 01 0004000000000000 02 7631 0d00000000000000 00 00 00 00 00",
	},
	{
		"NodeUpsertS3",
		Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{
			Idx: 2, ID: "fedcba9876543210", Name: "b", Kind: KindS3, State: Active,
			Capacity: 7, S3Endpoint: "e", S3Bucket: "b", S3Prefix: "p", S3Region: "r", S3PathStyle: true,
		}}},
		"01 03 0200 10 66656463626139383736353433323130 01 62 02 01 00 00 0000 0000 00 0700000000000000 00 0000000000000000 01 65 01 62 01 70 01 72 01",
	},
	{
		"NodeSetState",
		Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: 3, State: Draining, Now: 5}},
		"01 04 0300 02 0500000000000000",
	},
	{
		"Create",
		Command{Op: OpCreate, Create: &Create{Parent: 1, Name: "f", Type: TFile, Mode: 0o644, Uid: 501, Gid: 20, Now: 9}},
		"01 05 0100000000000000 01 66 01 a4010000 f5010000 14000000 00 0900000000000000",
	},
	{
		"Mkdir",
		Command{Op: OpMkdir, Mkdir: &Mkdir{Parent: 1, Name: "d", Mode: 0o755, Now: 3}},
		"01 06 0100000000000000 01 64 ed010000 00000000 00000000 0300000000000000",
	},
	{
		"SetVolumeMode",
		Command{Op: OpSetVolumeMode, SetVolumeMode: &SetVolumeMode{Auto: true, Now: 5}},
		"01 0e 01 0500000000000000",
	},
	{
		"Unlink",
		Command{Op: OpUnlink, Unlink: &Unlink{Parent: 1, Name: "a", Now: 2}},
		"01 07 0100000000000000 01 61 0200000000000000",
	},
	{
		"Rmdir",
		Command{Op: OpRmdir, Rmdir: &Rmdir{Parent: 1, Name: "d", Now: 4}},
		"01 08 0100000000000000 01 64 0400000000000000",
	},
	{
		"Rename",
		Command{Op: OpRename, Rename: &Rename{OldParent: 1, OldName: "a", NewParent: 2, NewName: "b", Now: 6}},
		"01 09 0100000000000000 01 61 0200000000000000 01 62 0600000000000000",
	},
	{
		"SetAttr",
		Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: 5, Mask: AttrMode | AttrMtime, Mode: 0o600, Mtime: 10, Now: 11}},
		"01 0a 0500000000000000 11 80010000 00000000 00000000 0000000000000000 0a00000000000000 0b00000000000000",
	},
	{
		"Commit",
		Command{Op: OpCommit, Commit: &Commit{
			Ino: 5, BaseVersion: 2, NewSize: 3,
			Writes:    []ChunkWrite{{Idx: 0, Hash: sampleHash(0xaa)}},
			NewChunks: []ChunkDesc{{Hash: sampleHash(0xaa), Size: 3, K: 1, M: 1, Placement: []NodeIdx{1, 2}}}, Now: 12,
		}},
		"01 0b 0500000000000000 0200000000000000 00 0000000000000000 0300000000000000 01 00000000 " + repeatHex(0xaa, 32) +
			" 01 " + repeatHex(0xaa, 32) + " 03000000 01 01 02 0100 0200 0c00000000000000",
	},
	{
		"CommitTruncate",
		Command{Op: OpCommit, Commit: &Commit{Ino: 5, BaseVersion: 2, HasTruncate: true, TruncateTo: 1, NewSize: 0, Now: 12}},
		"01 0b 0500000000000000 0200000000000000 01 0100000000000000 0000000000000000 00 00 0c00000000000000",
	},
	{
		"SetPlacement",
		Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: sampleHash(0xbb), Placement: []NodeIdx{1, 0, 3}, K: 2, M: 1}},
		"01 0c " + repeatHex(0xbb, 32) + " 03 0100 0000 0300 02 01",
	},
	{
		"GCDelete",
		Command{Op: OpGCDelete, GCDelete: &GCDelete{Before: -1}},
		"01 0d ffffffffffffffff",
	},
	{
		"SetPlacementExpect", // codec version 3: the placement the proposer planned from (I-102)
		Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: sampleHash(0xbb), Placement: []NodeIdx{1, 0, 3}, Expect: []NodeIdx{2, 0, 3}, K: 2, M: 1}},
		"03 0c " + repeatHex(0xbb, 32) + " 03 0100 0000 0300 03 0200 0000 0300 02 01",
	},
	{
		"GCDeleteLimit", // codec version 3: at most this many victims (I-103)
		Command{Op: OpGCDelete, GCDelete: &GCDelete{Before: -1, Limit: 1024}},
		"03 0d ffffffffffffffff 00040000",
	},
	{
		"Trash", // codec version 4 (docs/02 §Trash)
		Command{Op: OpTrash, Trash: &Trash{Parent: 1, Name: "d", Recursive: true, Now: 2}},
		"04 0f 0100000000000000 01 64 01 0200000000000000",
	},
	{
		"Restore",
		Command{Op: OpRestore, Restore: &Restore{Ino: 5, Now: 3}},
		"04 10 0500000000000000 0000000000000000 00 0300000000000000",
	},
	{
		"RestoreElsewhere",
		Command{Op: OpRestore, Restore: &Restore{Ino: 5, NewParent: 2, NewName: "b", Now: 3}},
		"04 10 0500000000000000 0200000000000000 01 62 0300000000000000",
	},
	{
		"Purge",
		Command{Op: OpPurge, Purge: &Purge{Before: -1, Limit: 1024, Now: 4}},
		"04 11 0000000000000000 ffffffffffffffff 00040000 0400000000000000",
	},
	{
		"SetAttrLibrary", // codec version 6: the library kind in the flags (I-210)
		Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: 7, Mask: AttrFlags, Flags: WithLibrary(FlagNSFW, LibraryMusic), Now: 9}},
		"060a0700000000000000200000000000000000000000000000000000000000000000000000000009000000000000000e",
	},
	{
		"SetShardSums", // codec version 5: a chunk's data-shard hashes (I-201)
		Command{Op: OpSetShardSums, SetShardSums: &SetShardSums{Hash: Hash{0xaa}, Sums: []Hash{{0x01}, {0x02}}}},
		"0512aa000000000000000000000000000000000000000000000000000000000000000201000000000000000000000000000000000000000000000000000000000000000200000000000000000000000000000000000000000000000000000000000000",
	},
	{
		"CommitWithSums", // codec version 5: each new chunk's sums follow its placement (I-201)
		Command{Op: OpCommit, Commit: &Commit{Ino: 2, BaseVersion: 1, NewSize: 64, Writes: []ChunkWrite{{0, Hash{0xbb}}}, NewChunks: []ChunkDesc{{Hash: Hash{0xbb}, Size: 64, K: 2, M: 1, Placement: []NodeIdx{1, 2, 3}, Sums: []Hash{{0x03}, {0x04}}}}, Now: 5}},
		"050b0200000000000000010000000000000000000000000000000040000000000000000100000000bb0000000000000000000000000000000000000000000000000000000000000001bb000000000000000000000000000000000000000000000000000000000000004000000002010301000200030002030000000000000000000000000000000000000000000000000000000000000004000000000000000000000000000000000000000000000000000000000000000500000000000000",
	},
	{
		"CommitRewriteStart", // codec version 9: Rewrite u64 and RewriteStart after Now (docs/04 §6, I-223)
		Command{Op: OpCommit, Commit: &Commit{Ino: 5, BaseVersion: 2, HasTruncate: true, NewSize: 0, Now: 12, RewriteStart: true}},
		"09 0b 0500000000000000 0200000000000000 01 0000000000000000 0000000000000000 00 00 0c00000000000000 0000000000000000 01",
	},
	{
		"CommitRewriteWithSums", // codec version 9 keeps the codec-5 sums (fields gate by version equality)
		Command{Op: OpCommit, Commit: &Commit{Ino: 2, BaseVersion: 1, NewSize: 64, Writes: []ChunkWrite{{0, Hash{0xbb}}}, NewChunks: []ChunkDesc{{Hash: Hash{0xbb}, Size: 64, K: 2, M: 1, Placement: []NodeIdx{1, 2, 3}, Sums: []Hash{{0x03}, {0x04}}}}, Now: 5, Rewrite: 4}},
		"090b0200000000000000010000000000000000000000000000000040000000000000000100000000bb0000000000000000000000000000000000000000000000000000000000000001bb000000000000000000000000000000000000000000000000000000000000004000000002010301000200030002030000000000000000000000000000000000000000000000000000000000000004000000000000000000000000000000000000000000000000000000000000000500000000000000 0400000000000000 00",
	},
	{
		"TrashExpect", // codec version 9: Own and Expect after Now (I-225)
		Command{Op: OpTrash, Trash: &Trash{Parent: 1, Name: "d", Recursive: true, Now: 2, Expect: 7}},
		"09 0f 0100000000000000 01 64 01 0200000000000000 00 0700000000000000",
	},
	{
		"TrashOwnExpect", // codec version 9 writes Own as a byte (in version 8 the version was the flag)
		Command{Op: OpTrash, Trash: &Trash{Parent: 1, Name: "d", Now: 2, Own: true, Expect: 7}},
		"09 0f 0100000000000000 01 64 00 0200000000000000 01 0700000000000000",
	},
	{
		"RenameNoReplace", // codec version 9: NoReplace after Now (I-224, I-226)
		Command{Op: OpRename, Rename: &Rename{OldParent: 1, OldName: "a", NewParent: 2, NewName: "b", Now: 6, NoReplace: true}},
		"09 09 0100000000000000 01 61 0200000000000000 01 62 0600000000000000 01",
	},
	{
		"SetAttrFlags", // codec version 4: Flags follows Now (I-156)
		Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: 5, Mask: AttrFlags, Flags: FlagProtected | FlagNSFW, Now: 11}},
		"04 0a 0500000000000000 20 00000000 00000000 00000000 0000000000000000 0000000000000000 0b00000000000000 03",
	},
}

// A codec-4 entry is written only for the ops that need it, and read only for them
// (docs/01 §Binary codec).
//
// Invariant: I-156
func TestEncodeCommand_V4Gating(t *testing.T) {
	plain := MustEncodeCommand(Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: 5, Mask: AttrMode, Mode: 1, Now: 1}})
	if plain[0] != commandVersion {
		t.Fatalf("a SetAttr without flags is version %d, want %d", plain[0], commandVersion)
	}
	flagged := MustEncodeCommand(Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: 5, Mask: AttrMode | AttrFlags, Mode: 1, Now: 1}})
	if flagged[0] != commandVersionV4 {
		t.Fatalf("a SetAttr with flags is version %d, want %d", flagged[0], commandVersionV4)
	}
	bad := map[string][]byte{
		"trash in version 1":           append([]byte{commandVersion}, MustEncodeCommand(Command{Op: OpTrash, Trash: &Trash{Parent: 1, Name: "d", Now: 2}})[1:]...),
		"unlink in version 4":          append([]byte{commandVersionV4}, MustEncodeCommand(Command{Op: OpUnlink, Unlink: &Unlink{Parent: 1, Name: "a", Now: 2}})[1:]...),
		"version 4 SetAttr sans flags": append([]byte{commandVersionV4}, append(plain[1:], 0)...),
	}
	for name, in := range bad {
		if _, err := DecodeCommand(in); err == nil {
			t.Errorf("%s: accepted %x", name, in)
		}
	}
}

// Invariant: I-22, I-156
func TestEncodeCommand_Golden(t *testing.T) {
	for _, g := range goldenCommands {
		t.Run(g.name, func(t *testing.T) {
			got := MustEncodeCommand(g.cmd)
			if want := h(g.hex); !bytes.Equal(got, want) {
				t.Fatalf("encode\n got %x\nwant %x", got, want)
			}
			dec, err := DecodeCommand(got)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(normalizeCommand(dec), normalizeCommand(g.cmd)) {
				t.Fatalf("round trip\n got %+v\nwant %+v", dec, g.cmd)
			}
		})
	}
}

// normalizeCommand makes nil and empty slices compare equal.
func normalizeCommand(c Command) Command {
	if c.Commit != nil {
		cc := *c.Commit
		if len(cc.Writes) == 0 {
			cc.Writes = nil
		}
		if len(cc.NewChunks) == 0 {
			cc.NewChunks = nil
		}
		for i := range cc.NewChunks {
			if len(cc.NewChunks[i].Placement) == 0 {
				cc.NewChunks[i].Placement = nil
			}
		}
		c.Commit = &cc
	}
	if c.SetPlacement != nil && len(c.SetPlacement.Placement) == 0 {
		sp := *c.SetPlacement
		sp.Placement = nil
		c.SetPlacement = &sp
	}
	return c
}

func TestEncodeCommand_AllOpsCovered(t *testing.T) {
	seen := map[Op]bool{}
	for _, g := range goldenCommands {
		seen[g.cmd.Op] = true
	}
	for op := OpSetVolume; op <= OpSnapshotDelete; op++ {
		if !seen[op] {
			t.Errorf("op %d has no golden", op)
		}
	}
}

func TestDecodeCommand_Rejects(t *testing.T) {
	good := MustEncodeCommand(Command{Op: OpUnlink, Unlink: &Unlink{Parent: 1, Name: "a", Now: 2}})
	cases := map[string][]byte{
		"empty":              {},
		"version only":       {1},
		"bad version":        append([]byte{2}, good[1:]...),
		"unknown op":         {1, 200},
		"op zero":            {1, 0},
		"truncated":          good[:len(good)-1],
		"trailing garbage":   append(append([]byte{}, good...), 0),
		"huge string len":    {1, 7, 1, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0x0f},
		"missing payload":    {1, 13},
		"non-minimal varint": {1, 7, 1, 0, 0, 0, 0, 0, 0, 0, 0x80, 0x00, 2, 0, 0, 0, 0, 0, 0, 0},
	}
	for name, in := range cases {
		if _, err := DecodeCommand(in); err == nil {
			t.Errorf("%s: accepted %x", name, in)
		}
	}
}

func TestEncodeCommand_MissingPayloadPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for Op without payload")
		}
	}()
	MustEncodeCommand(Command{Op: OpUnlink})
}

func TestResult_RoundTrip(t *testing.T) {
	cases := []Result{
		{},
		{Ino: 7, Gen: 9},
		{Err: ErrConflict},
		{Victims: []GCVictim{{Hash: sampleHash(1), Placement: []NodeIdx{1, 0, 2}}, {Hash: sampleHash(2)}}},
	}
	for _, r := range cases {
		b := EncodeResult(r)
		got, err := DecodeResult(b)
		if err != nil {
			t.Fatalf("decode %+v: %v", r, err)
		}
		if got.Ino != r.Ino || got.Gen != r.Gen || (got.Err == nil) != (r.Err == nil) || (r.Err != nil && got.Err.Error() != r.Err.Error()) {
			t.Fatalf("got %+v want %+v", got, r)
		}
		if len(got.Victims) != len(r.Victims) {
			t.Fatalf("victims %d want %d", len(got.Victims), len(r.Victims))
		}
		for i := range r.Victims {
			if got.Victims[i].Hash != r.Victims[i].Hash || !equalIdx(got.Victims[i].Placement, r.Victims[i].Placement) {
				t.Fatalf("victim %d: %+v want %+v", i, got.Victims[i], r.Victims[i])
			}
		}
	}
	if _, err := DecodeResult([]byte{99}); err == nil {
		t.Fatal("unknown error code accepted")
	}
	if _, err := DecodeResult(append(EncodeResult(Result{}), 0)); err == nil {
		t.Fatal("trailing garbage accepted")
	}
}

func equalIdx(a, b []NodeIdx) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Invariant: I-22, I-26, I-156
func FuzzDecodeCommand(f *testing.F) {
	for i := range goldenCommands {
		f.Add(h(goldenCommands[i].hex))
	}
	f.Add([]byte{})
	f.Add([]byte{1})
	f.Fuzz(func(t *testing.T, in []byte) {
		c, err := DecodeCommand(in)
		if err != nil {
			return
		}
		out := MustEncodeCommand(c)
		if !bytes.Equal(out, in) {
			t.Fatalf("decode/encode not identity: %x -> %x", in, out)
		}
	})
}
