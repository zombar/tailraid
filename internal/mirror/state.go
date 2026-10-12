package mirror

import (
	"context"
	"fmt"

	"github.com/zombar/tailraid/internal/meta"
)

// StateSource adapts the FSM clone of a snapshot's index to the writer: the tree
// reachable from the root, without /.tailraid and the ephemeral names (docs/16 §3). The
// trash is not in it, a deleted root being out of its parent's name table. read fetches a
// chunk's bytes (the host's reader).
func StateSource(st *meta.State, read func(ctx context.Context, h meta.Hash) ([]byte, error)) Source {
	return stateSource{st: st, read: read}
}

type stateSource struct {
	st   *meta.State
	read func(ctx context.Context, h meta.Hash) ([]byte, error)
}

func (s stateSource) Dir(ino uint64) ([]Entry, error) {
	dir, ok := s.st.Inodes[meta.Ino(ino)]
	if !ok || dir.Type != meta.TDir {
		return nil, fmt.Errorf("no directory %d", ino)
	}
	out := make([]Entry, 0, len(dir.Children))
	for name, child := range dir.Children {
		if meta.IsEphemeral(name) || (dir.Ino == meta.RootIno && name == meta.ReservedDir) {
			continue
		}
		in, ok := s.st.Inodes[child]
		if !ok {
			return nil, fmt.Errorf("directory %d names a missing inode %d", ino, child)
		}
		e := Entry{Name: name, Type: EntryType(in.Type), Mode: in.Mode, Uid: in.Uid, Gid: in.Gid, Mtime: in.Mtime, Ino: uint64(in.Ino), Gen: in.Gen}
		switch in.Type {
		case meta.TFile:
			e.Size, e.Chunks = in.Size, in.Chunks
		case meta.TSymlink:
			e.Target = in.Target
		case meta.TDir:
		}
		out = append(out, e)
	}
	return out, nil
}

func (s stateSource) Chunk(ctx context.Context, h meta.Hash) ([]byte, error) { return s.read(ctx, h) }
