package vfs

import (
	"context"
	"errors"

	"github.com/zombar/tailraid/internal/meta"
)

// The browser UI's trash operations (docs/13 §3, §4). Unlike the billy methods these
// return the meta sentinels unmapped, so the web handler can tell a taken name
// (ErrExist) from a parent in the trash (ErrInTrash) and a protected entry
// (ErrProtected) from a missing one, each with its own status.

// Trash moves the entry at filename into the trash, a folder with everything under it
// (the share's Remove is not recursive). An ephemeral name or anything under /.tailraid
// is unlinked for good. While a member is too old for the trash a file falls back to
// the plain unlink; a folder cannot and the caller sees ErrCodec.
func (f *FS) Trash(filename string) error {
	ctx := context.Background()
	dp, name := parentOf(meta.SplitPath(filename))
	if name == "" {
		return meta.ErrBadCommand
	}
	dir, err := f.st.Resolve(dp)
	if err != nil {
		return err
	}
	in, ok := f.st.Lookup(dir.Ino, name)
	if !ok {
		return meta.ErrNotExist
	}
	hard := hardDelete(dp, name)
	_, err = f.propose(ctx, meta.Command{Op: meta.OpTrash, Trash: &meta.Trash{Parent: dir.Ino, Name: name, Recursive: true, Now: f.now()}})
	if errors.Is(err, meta.ErrCodec) && in.Type != meta.TDir {
		hard = true
		_, err = f.propose(ctx, meta.Command{Op: meta.OpUnlink, Unlink: &meta.Unlink{Parent: dir.Ino, Name: name, Now: f.now()}})
	}
	if err != nil && !landedEarlier(err, meta.ErrNotExist) {
		return err
	}
	if hard {
		f.ov.Drop(in.Ino)
	}
	return nil
}

// Restore puts the deleted root ino back: under newParent/newName, or (0, "") in its
// own place. ErrExist says the name is taken, ErrInTrash that the destination is
// itself deleted, ErrNotExist that ino is not a deleted root.
func (f *FS) Restore(ino, newParent meta.Ino, newName string) error {
	_, err := f.propose(context.Background(), meta.Command{Op: meta.OpRestore, Restore: &meta.Restore{Ino: ino, NewParent: newParent, NewName: newName, Now: f.now()}})
	if landedEarlier(err, meta.ErrNotExist) {
		return nil
	}
	return err
}

// Purge deletes the deleted root ino for good: its references are dropped and GC
// reclaims its chunks after grace (docs/06).
func (f *FS) Purge(ino meta.Ino) error {
	_, err := f.propose(context.Background(), meta.Command{Op: meta.OpPurge, Purge: &meta.Purge{Ino: ino, Now: f.now()}})
	if landedEarlier(err, meta.ErrNotExist) {
		return nil
	}
	return err
}

// PurgeBatch bounds one purge entry: a large trash drains in several.
const PurgeBatch = 1000

// PurgeAll empties the trash, however young its entries, in entries of PurgeBatch
// inodes until nothing is left.
func (f *FS) PurgeAll() error {
	ctx := context.Background()
	for {
		left := f.st.Stats().TrashInodes
		if left == 0 {
			return nil
		}
		before := max(f.now(), f.st.LastNow()) + 1
		if _, err := f.propose(ctx, meta.Command{Op: meta.OpPurge, Purge: &meta.Purge{Before: before, Limit: PurgeBatch, Now: f.now()}}); err != nil {
			return err
		}
		if f.st.Stats().TrashInodes >= left {
			return errors.New("vfs: purge made no progress")
		}
	}
}

// SetFlags sets a directory's protected and nsfw flags (docs/01, docs/13 §4).
func (f *FS) SetFlags(filename string, flags uint8) error {
	in, err := f.resolve(filename)
	if err != nil {
		return err
	}
	_, err = f.propose(context.Background(), meta.Command{Op: meta.OpSetAttr, SetAttr: &meta.SetAttr{Ino: in.Ino, Mask: meta.AttrFlags, Flags: flags, Now: f.now()}})
	return err
}
