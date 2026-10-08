package meta

import (
	"bytes"
	"reflect"
	"testing"
)

func TestNewState_HasRoot(t *testing.T) {
	s := NewState()
	root, ok := s.Inodes[RootIno]
	if !ok {
		t.Fatal("root inode missing")
	}
	if root.Type != TDir || root.Parent != 0 || root.Name != "" || root.Nlink != 2 {
		t.Fatalf("root = %+v", *root)
	}
	if root.Children == nil {
		t.Fatal("root.Children nil")
	}
	if s.NextIno != RootIno+1 || s.NextNode != 1 {
		t.Fatalf("NextIno=%d NextNode=%d", s.NextIno, s.NextNode)
	}
	for _, m := range []any{s.Chunks, s.Nodes, s.Tombs} {
		if reflect.ValueOf(m).IsNil() {
			t.Fatalf("map %T nil", m)
		}
	}
}

// Invariant: I-3
func TestInoGen_RootIsOne_ZeroIsNever(t *testing.T) {
	if RootIno != 1 {
		t.Fatalf("RootIno = %d", RootIno)
	}
	s := NewState()
	if _, ok := s.Inodes[0]; ok {
		t.Fatal("ino 0 must never exist")
	}
}

func TestState_Clone_IsDeep(t *testing.T) {
	s := sampleState()
	c := s.Clone()
	if !reflect.DeepEqual(s, c) {
		t.Fatal("clone not equal")
	}
	// Mutate clone maps and nested maps; original must not change.
	// Chunks slices are immutable and are shared by design: same backing array.
	if &s.Inodes[2].Chunks[0] != &c.Inodes[2].Chunks[0] {
		t.Fatal("Chunks slice was copied; the documented choice is to share it")
	}
	c.Inodes[RootIno].Children["new"] = 99
	c.Inodes[RootIno].Name = "x"
	c.Nodes[1].Name = "renamed"
	c.Chunks[sampleHash(1)].Refs = 42
	c.Tombs[sampleHash(9)] = struct{}{}
	delete(c.Inodes, 2)
	if _, ok := s.Inodes[RootIno].Children["new"]; ok {
		t.Fatal("Children map shared")
	}
	if s.Inodes[RootIno].Name != "" || s.Nodes[1].Name != "node1" || s.Chunks[sampleHash(1)].Refs != 1 {
		t.Fatal("struct pointers shared")
	}
	if _, ok := s.Tombs[sampleHash(9)]; ok {
		t.Fatal("Tombs shared")
	}
	if _, ok := s.Inodes[2]; !ok {
		t.Fatal("Inodes map shared")
	}
}

func TestChunkIndex(t *testing.T) {
	cases := []struct {
		size, chunk uint64
		want        int
	}{
		{0, 1 << 20, 0}, {1, 1 << 20, 1}, {1<<20 - 1, 1 << 20, 1}, {1 << 20, 1 << 20, 1}, {1<<20 + 1, 1 << 20, 2},
	}
	for _, c := range cases {
		if got := ChunkCount(c.size, uint32(c.chunk)); got != c.want {
			t.Errorf("ChunkCount(%d,%d)=%d want %d", c.size, c.chunk, got, c.want)
		}
	}
}

// sampleState builds a small but complete state used across tests.
func sampleState() *State {
	s := NewState()
	s.Vol = Volume{ChunkSize: 1 << 20, K: 2, M: 1, PolicyVersion: 1}
	s.Meta = Meta{ClusterID: [16]byte{1, 2, 3}, JoinToken: "abcdefgh", CreatedAt: 100}
	s.Applied = 7
	s.LastNow = 99
	s.Nodes[1] = &Node{Idx: 1, ID: "0123456789abcdef", Name: "node1", Kind: KindDevice, State: Active,
		Capacity: 1 << 40, Voter: true, TSStableID: "27", Addr: "100.64.0.11", RaftPort: 20480, RPCPort: 20481, Version: "v1", JoinedAt: 5}
	s.Nodes[2] = &Node{Idx: 2, ID: "fedcba9876543210", Name: "bucket", Kind: KindS3, State: Active,
		Capacity: 1 << 41, S3Endpoint: "https://s3.example", S3Bucket: "b", S3Prefix: "p/", S3Region: "us-west-004", S3PathStyle: true, JoinedAt: 6}
	s.NextNode = 3
	s.Chunks[sampleHash(1)] = &Chunk{Hash: sampleHash(1), Size: 1 << 20, K: 2, M: 1, Refs: 1, Placement: []NodeIdx{1, 2, 0}}
	s.Chunks[sampleHash(2)] = &Chunk{Hash: sampleHash(2), Size: 10, K: 1, M: 1, Refs: 0, Placement: []NodeIdx{2, 1}, TombstonedAt: 50}
	s.Tombs[sampleHash(2)] = struct{}{}
	s.Inodes[2] = &Inode{Ino: 2, Gen: 3, Type: TFile, Mode: 0o644, Uid: 501, Gid: 20, Size: 1<<20 + 5,
		Atime: 1, Mtime: 2, Ctime: 3, Nlink: 1, Version: 4, Parent: RootIno, Name: "f", Chunks: []Hash{sampleHash(1), HashZero}}
	s.Inodes[3] = &Inode{Ino: 3, Gen: 4, Type: TDir, Mode: 0o755, Nlink: 2, Parent: RootIno, Name: "d", Children: map[string]Ino{}}
	s.Inodes[4] = &Inode{Ino: 4, Gen: 5, Type: TSymlink, Mode: 0o777, Nlink: 1, Parent: RootIno, Name: "l", Target: "/f"}
	s.Inodes[RootIno].Children = map[string]Ino{"f": 2, "d": 3, "l": 4}
	s.NextIno = 5
	return s
}

func sampleHash(b byte) Hash {
	var h Hash
	for i := range h {
		h[i] = b
	}
	return h
}

// Invariant: I-16
func TestHandle_RoundTrip_BigEndian(t *testing.T) {
	b := EncodeHandle(0x0102030405060708, 0xaabbccdd)
	want := []byte{0x54, 0x52, 0x48, 0x44, 0xaa, 0xbb, 0xcc, 0xdd, 1, 2, 3, 4, 5, 6, 7, 8}
	if !bytes.Equal(b, want) {
		t.Fatalf("handle %x want %x", b, want)
	}
	ino, gen, ok := DecodeHandle(b)
	if !ok || ino != 0x0102030405060708 || gen != 0xaabbccdd {
		t.Fatalf("decode %d %x %v", ino, gen, ok)
	}
	for _, bad := range [][]byte{nil, b[:15], append(append([]byte{}, b...), 0), append([]byte{0}, b[1:]...)} {
		if _, _, ok := DecodeHandle(bad); ok {
			t.Errorf("accepted %x", bad)
		}
	}
}
