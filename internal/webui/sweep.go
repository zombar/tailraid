package webui

import (
	"context"
	"path"
	"time"

	"github.com/zombar/tailraid/internal/meta"
)

// Remover is what the sweep needs from the vfs.
type Remover interface {
	Remove(name string) error
}

// SweepUploads removes every upload temp whose mtime is olderThan or more before now:
// an upload nobody resumed (docs/13 §4, I-81). It walks the FSM, so it costs nothing on
// a volume without temps beyond the directory walk.
func SweepUploads(ctx context.Context, fs Remover, st *meta.Store, now time.Time, olderThan time.Duration) (int, error) {
	type dir struct {
		ino  meta.Ino
		path string
	}
	stack := []dir{{meta.RootIno, "/"}}
	removed := 0
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		d := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		kids, err := st.ReadDir(d.ino)
		if err != nil {
			continue // unlinked meanwhile
		}
		for i := range kids {
			k := &kids[i]
			switch {
			case k.Type == meta.TDir:
				stack = append(stack, dir{k.Ino, path.Join(d.path, k.Name)})
			case k.Type == meta.TFile && IsUploadTemp(k.Name) && now.Sub(time.Unix(0, k.Mtime)) >= olderThan:
				if err := fs.Remove(path.Join(d.path, k.Name)); err != nil {
					return removed, err
				}
				removed++
			}
		}
	}
	return removed, nil
}
