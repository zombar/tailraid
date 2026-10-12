package meta

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"slices"
	"sort"
)

var stateMagic = []byte("TRM1")

// stateVersion 2 adds Volume.Auto, 3 Node.Host, 4 Inode.Flags and DeletedAt, 5 Chunk.Sums,
// 7 the snapshot catalogue and 9 Inode.RewriteAt; older
// snapshots are still read (the fields zero) so a cluster upgrades without a format
// break (docs/12 §6, I-156).
const (
	stateVersion   = stateVersionV9 // the newest written; 6 and 8 are never written: those log codecs added no state
	stateVersionV9 = 9              // Inode.RewriteAt; gate a field on its own version, never on stateVersion
	stateVersionV7 = 7              // the snapshot catalogue (docs/16, I-184)
	stateVersionV5 = 5
	stateVersionV4 = 4
	stateVersionV3 = 3
	stateVersionV2 = 2
	stateVersionV1 = 1
)

// EncodeState writes a canonical snapshot of s: maps in sorted key order, so equal states
// produce byte-identical output (I-22). Tombs is not written; it is rebuilt on decode.
func EncodeState(wr io.Writer, s *State) error { return encodeStateVersion(wr, s, stateVersion) }

// encodeStateVersion writes s in the given snapshot version; only the current one is
// ever written in production, and the older ones exist so the decoder's compatibility
// paths are tested against real bytes.
func encodeStateVersion(wr io.Writer, s *State, version uint8) error {
	w := &writer{b: make([]byte, 0, 64<<10), out: wr}
	w.b = append(w.b, stateMagic...)
	w.u8(version)
	w.u64(s.Applied)
	w.i64(s.LastNow)
	w.u32(s.Vol.ChunkSize)
	w.u8(s.Vol.K)
	w.u8(s.Vol.M)
	w.u32(s.Vol.PolicyVersion)
	if version >= stateVersionV2 {
		w.bool(s.Vol.Auto)
	}
	w.b = append(w.b, s.Meta.ClusterID[:]...)
	w.str(s.Meta.JoinToken)
	w.i64(s.Meta.CreatedAt)
	w.u64(uint64(s.NextIno))
	w.u16(uint16(s.NextNode))

	nodeIdx := make([]int, 0, len(s.Nodes))
	for idx := range s.Nodes {
		nodeIdx = append(nodeIdx, int(idx))
	}
	sort.Ints(nodeIdx)
	w.uvarint(uint64(len(nodeIdx)))
	for _, idx := range nodeIdx {
		writeNode(w, s.Nodes[NodeIdx(idx)], version >= stateVersionV3)
		w.spill()
	}

	hashes := make([]Hash, 0, len(s.Chunks))
	for h := range s.Chunks {
		hashes = append(hashes, h)
	}
	sort.Slice(hashes, func(i, j int) bool { return bytes.Compare(hashes[i][:], hashes[j][:]) < 0 })
	w.uvarint(uint64(len(hashes)))
	for _, h := range hashes {
		c := s.Chunks[h]
		w.hash(h)
		w.u32(c.Size)
		w.u8(c.K)
		w.u8(c.M)
		w.u32(c.Refs)
		w.nodeIdxs(c.Placement)
		w.i64(c.TombstonedAt)
		if version >= stateVersionV5 {
			w.hashes(c.Sums)
		}
		w.spill()
	}

	inos := make([]uint64, 0, len(s.Inodes))
	for ino := range s.Inodes {
		inos = append(inos, uint64(ino))
	}
	slices.Sort(inos)
	w.uvarint(uint64(len(inos)))
	for _, ino := range inos {
		in := s.Inodes[Ino(ino)]
		w.u64(uint64(in.Ino))
		w.u32(in.Gen)
		w.u8(uint8(in.Type))
		w.u32(in.Mode)
		w.u32(in.Uid)
		w.u32(in.Gid)
		w.u64(in.Size)
		w.i64(in.Atime)
		w.i64(in.Mtime)
		w.i64(in.Ctime)
		w.u16(in.Nlink)
		w.u64(in.Version)
		w.u64(uint64(in.Parent))
		w.str(in.Name)
		if version >= stateVersionV4 {
			w.u8(in.Flags)
			w.i64(in.DeletedAt)
		}
		if version >= stateVersionV9 {
			w.u64(in.RewriteAt)
		}
		switch in.Type {
		case TFile:
			w.uvarint(uint64(len(in.Chunks)))
			for _, h := range in.Chunks {
				w.hash(h)
			}
		case TSymlink:
			w.str(in.Target)
		case TDir:
			names := make([]string, 0, len(in.Children))
			for name := range in.Children {
				names = append(names, name)
			}
			sort.Strings(names)
			w.uvarint(uint64(len(names)))
			for _, name := range names {
				w.str(name)
				w.u64(uint64(in.Children[name]))
			}
		default:
			return fmt.Errorf("meta: inode %d has invalid type %d", in.Ino, in.Type)
		}
		w.spill()
	}
	if version >= stateVersionV7 {
		writeSnaps(w, s)
	}
	return w.flush()
}

// writeSnaps writes the snapshot catalogue in ID order, then NextSnap (version 7).
func writeSnaps(w *writer, s *State) {
	ids := make([]SnapID, 0, len(s.Snaps))
	for id := range s.Snaps {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	w.uvarint(uint64(len(ids)))
	for _, id := range ids {
		sn := s.Snaps[id]
		w.u32(uint32(sn.ID))
		w.u16(uint16(sn.Mirror))
		w.str(sn.Name)
		w.u64(sn.Index)
		w.i64(sn.At)
		w.bool(sn.Auto)
		w.u8(uint8(sn.State))
		w.hash(sn.Root)
		w.u64(sn.Files)
		w.u64(sn.Bytes)
		w.u64(sn.NewBytes)
		w.u32(sn.Missing)
	}
	w.u32(uint32(s.NextSnap))
}

// readSnaps is the inverse of writeSnaps; IDs must ascend and states be known.
func readSnaps(r *reader, s *State) error {
	n := r.length(maxListLen)
	var prev SnapID
	for i := 0; i < n && r.err == nil; i++ {
		sn := &Snapshot{
			ID: SnapID(r.u32()), Mirror: NodeIdx(r.u16()), Name: r.str(), Index: r.u64(), At: r.i64(), Auto: r.bool(),
			State: SnapState(r.u8()), Root: r.hash(), Files: r.u64(), Bytes: r.u64(), NewBytes: r.u64(), Missing: r.u32(),
		}
		if r.err == nil && (sn.ID == 0 || sn.ID <= prev) {
			return fmt.Errorf("meta: snapshot catalogue not in canonical order")
		}
		if r.err == nil && (sn.State < SnapSyncing || sn.State > SnapDeleting) {
			return fmt.Errorf("meta: snapshot %d has invalid state %d", sn.ID, sn.State)
		}
		prev = sn.ID
		s.Snaps[sn.ID] = sn
	}
	s.NextSnap = SnapID(r.u32())
	return nil
}

// DecodeState parses a snapshot written by EncodeState. It rejects unknown versions,
// truncation, trailing bytes, and structurally invalid states (HashZero chunk entries,
// invalid inode types, non-canonical order).
func DecodeState(rd io.Reader) (*State, error) {
	r := &reader{src: bufio.NewReaderSize(rd, 1<<20)} // streamed: never the whole snapshot in memory at once (I-104)
	if !bytes.Equal(r.take(4), stateMagic) {
		return nil, fmt.Errorf("meta: bad snapshot magic")
	}
	version := r.u8()
	if r.err == nil && version != stateVersion && version != stateVersionV7 && version != stateVersionV5 && version != stateVersionV4 && version != stateVersionV3 && version != stateVersionV2 && version != stateVersionV1 {
		return nil, fmt.Errorf("meta: unsupported snapshot version %d", version)
	}
	s := &State{
		Inodes: map[Ino]*Inode{},
		Chunks: map[Hash]*Chunk{},
		Nodes:  map[NodeIdx]*Node{},
		Tombs:  map[Hash]struct{}{},
		Trash:  map[Ino]map[Ino]struct{}{},
		Snaps:  map[SnapID]*Snapshot{},
	}
	s.Applied = r.u64()
	s.LastNow = r.i64()
	s.Vol = Volume{ChunkSize: r.u32(), K: r.u8(), M: r.u8(), PolicyVersion: r.u32()}
	if version >= stateVersionV2 {
		s.Vol.Auto = r.bool()
	}
	copy(s.Meta.ClusterID[:], r.take(16))
	s.Meta.JoinToken = r.str()
	s.Meta.CreatedAt = r.i64()
	s.NextIno = Ino(r.u64())
	s.NextNode = NodeIdx(r.u16())

	n := r.length(maxListLen)
	var prevIdx NodeIdx
	for i := 0; i < n && r.err == nil; i++ {
		node := readNode(r, version >= stateVersionV3)
		if i > 0 && node.Idx <= prevIdx {
			return nil, fmt.Errorf("meta: nodes not in canonical order")
		}
		prevIdx = node.Idx
		nn := node
		s.Nodes[node.Idx] = &nn
	}

	n = r.length(maxListLen)
	var prevHash Hash
	for i := 0; i < n && r.err == nil; i++ {
		h := r.hash()
		if i > 0 && bytes.Compare(h[:], prevHash[:]) <= 0 {
			return nil, fmt.Errorf("meta: chunks not in canonical order")
		}
		prevHash = h
		c := &Chunk{Hash: h, Size: r.u32(), K: r.u8(), M: r.u8(), Refs: r.u32(), Placement: r.nodeIdxs(), TombstonedAt: r.i64()}
		if version >= stateVersionV5 {
			c.Sums = r.hashes()
		}
		if h == HashZero {
			return nil, fmt.Errorf("meta: snapshot contains a HashZero chunk entry")
		}
		s.Chunks[h] = c
		if c.Refs == 0 {
			s.Tombs[h] = struct{}{}
		}
	}

	n = r.length(maxListLen)
	var prevIno Ino
	for i := 0; i < n && r.err == nil; i++ {
		in := &Inode{
			Ino: Ino(r.u64()), Gen: r.u32(), Type: FileType(r.u8()), Mode: r.u32(), Uid: r.u32(), Gid: r.u32(),
			Size: r.u64(), Atime: r.i64(), Mtime: r.i64(), Ctime: r.i64(), Nlink: r.u16(), Version: r.u64(), Parent: Ino(r.u64()), Name: r.str(),
		}
		if i > 0 && in.Ino <= prevIno {
			return nil, fmt.Errorf("meta: inodes not in canonical order")
		}
		prevIno = in.Ino
		if version >= stateVersionV4 {
			in.Flags = r.u8()
			in.DeletedAt = r.i64()
		}
		if version >= stateVersionV9 {
			in.RewriteAt = r.u64()
		}
		switch in.Type {
		case TFile:
			if cnt := r.length(maxListLen); cnt > 0 {
				in.Chunks = make([]Hash, cnt)
				for j := range in.Chunks {
					in.Chunks[j] = r.hash()
				}
			}
		case TSymlink:
			in.Target = r.str()
		case TDir:
			cnt := r.length(maxListLen)
			in.Children = make(map[string]Ino, cnt)
			prev := ""
			for j := 0; j < cnt && r.err == nil; j++ {
				name := r.str()
				if j > 0 && name <= prev {
					return nil, fmt.Errorf("meta: children not in canonical order")
				}
				prev = name
				in.Children[name] = Ino(r.u64())
			}
		default:
			if r.err == nil {
				return nil, fmt.Errorf("meta: inode %d has invalid type %d", in.Ino, in.Type)
			}
		}
		s.Inodes[in.Ino] = in
		if in.DeletedAt != 0 {
			s.indexTrash(in.Parent, in.Ino) // the trash index is rebuilt, like Tombs (I-156)
		}
	}
	if version >= stateVersionV7 {
		if err := readSnaps(r, s); err != nil {
			return nil, err
		}
	} else {
		s.NextSnap = 1 // an older snapshot has no catalogue (I-184)
	}
	if err := r.done(); err != nil {
		return nil, err
	}
	return s, nil
}
