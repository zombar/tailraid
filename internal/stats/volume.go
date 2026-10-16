package stats

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/vfs"
)

// VFS is the Volume of a running node: writes go through the vfs like an upload's (a temp
// beside the target, flushed, then renamed, I-192), reads through a reader that consults
// the chunk cache, where the system pin keeps the segments, but never fills it and never
// reads ahead (Reader.NoFill, I-196, I-373), listings through the FSM.
type VFS struct {
	FS *vfs.FS
	St *meta.Store
	Rd *vfs.Reader // NoFill, or with no cache (the audit log)
}

// WriteFile writes data whole: temp, flush, rename.
func (v VFS) WriteFile(ctx context.Context, name string, data []byte) error {
	dir, base := path.Split(name)
	temp := path.Join(dir, "."+base+".tailraid-stats")
	f, err := v.FS.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("stats segment: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = v.FS.Remove(temp)
		return fmt.Errorf("stats segment: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("stats segment: %w", err)
	}
	if err := v.FS.Flush(ctx, temp); err != nil {
		_ = v.FS.Remove(temp)
		return fmt.Errorf("stats segment flush: %w", err)
	}
	if err := v.FS.Rename(temp, name); err != nil {
		_ = v.FS.Remove(temp)
		return fmt.Errorf("stats segment: %w", err)
	}
	return nil
}

// ReadFile reads a committed file, filling no chunk cache.
func (v VFS) ReadFile(ctx context.Context, name string) ([]byte, error) {
	in, err := v.St.Resolve(meta.SplitPath(name))
	if err != nil {
		return nil, err
	}
	buf := make([]byte, in.Size)
	n, err := v.Rd.ReadAt(ctx, in, buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}

// List names a directory's entries; a missing directory has none.
func (v VFS) List(_ context.Context, dir string) ([]string, error) {
	in, err := v.St.Resolve(meta.SplitPath(dir))
	if errors.Is(err, meta.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	kids, err := v.St.ReadDir(in.Ino)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(kids))
	for i := range kids {
		names = append(names, kids[i].Name)
	}
	return names, nil
}

// Remove unlinks a file.
func (v VFS) Remove(_ context.Context, name string) error { return v.FS.Remove(name) }

// Version names a file's current content, `<ino>.<gen>.<version>` as the web UI's ETag:
// a sealed segment keeps it for life, the month segment changes it with every rebuild.
func (v VFS) Version(_ context.Context, name string) (string, error) {
	in, err := v.St.Resolve(meta.SplitPath(name))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d.%d.%d", in.Ino, in.Gen, in.Version), nil
}
