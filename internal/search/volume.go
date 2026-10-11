package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/vfs"
)

// VFS is the Volume of a running node: writes go through the vfs like an upload's (a
// temp beside the target, flushed, then renamed), reads through a reader with no chunk
// cache and no readahead, under the background mark (docs/06 §4, I-175).
type VFS struct {
	FS *vfs.FS
	St *meta.Store
	Rd *vfs.Reader // vfs.NewReader(..., nil cache, ...)
}

func (v VFS) Create(_ context.Context, name string) error {
	f, err := v.FS.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("search segment: %w", err)
	}
	return f.Close()
}

func (v VFS) WriteFile(ctx context.Context, name string, data []byte) error {
	dir, base := path.Split(name)
	temp := path.Join(dir, "."+base+".tailraid-search")
	f, err := v.FS.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("search segment: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = v.FS.Remove(temp)
		return fmt.Errorf("search segment: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("search segment: %w", err)
	}
	if err := v.FS.Flush(ctx, temp); err != nil {
		_ = v.FS.Remove(temp)
		return fmt.Errorf("search segment flush: %w", err)
	}
	if err := v.FS.Rename(temp, name); err != nil {
		_ = v.FS.Remove(temp)
		return fmt.Errorf("search segment: %w", err)
	}
	return nil
}

func (v VFS) ReadFile(ctx context.Context, name string) ([]byte, error) {
	in, err := v.St.Resolve(meta.SplitPath(name))
	if err != nil {
		return nil, err
	}
	return v.ReadPrefix(ctx, in, int(min(in.Size, 1<<40))) //nolint:gosec // bounded
}

func (v VFS) ReadPrefix(ctx context.Context, in meta.Inode, n int) ([]byte, error) {
	buf := make([]byte, min(uint64(max(n, 0)), in.Size)) //nolint:gosec // n ≥ 0
	got, err := v.Rd.ReadAt(chunk.Background(ctx), in, buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:got], nil
}

func (v VFS) Remove(_ context.Context, name string) error { return v.FS.Remove(name) }
