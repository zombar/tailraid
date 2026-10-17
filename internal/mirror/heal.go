package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/zombar/tailraid/internal/meta"
)

// Heal shortens snapshot id's missing list (docs/16 §5, I-391): a chunk the mirror holds
// as an object leaves it unread, any other is read through read, verified against its
// hash and written, and one still unreadable stays. When the list shrank the manifest is
// rewritten in place, keeping everything but the list, before Heal returns; it returns
// how many chunks the manifest still lists. A mirror a sync writes to is left alone.
func (m *Mirror) Heal(ctx context.Context, id uint32, read func(context.Context, meta.Hash) ([]byte, error)) (left int, err error) {
	b, err := readAll(m.fsys, m.manifestPath(id))
	if err != nil {
		return 0, err
	}
	var man Manifest
	if err := json.Unmarshal(b, &man); err != nil {
		return 0, fmt.Errorf("mirror: manifest %d: %w", id, err)
	}
	if len(man.Missing) == 0 || m.writing() {
		return len(man.Missing), nil
	}
	was := len(man.Missing) // what the manifest lists until it is rewritten
	var still []HexHash
	for _, hh := range man.Missing {
		if ctx.Err() != nil {
			return was, ctx.Err()
		}
		h := meta.Hash(hh)
		if m.HasObject(h) {
			continue
		}
		data, err := read(ctx, h)
		if err != nil || sha256.Sum256(data) != h {
			still = append(still, hh)
			continue
		}
		if _, err := m.PutObject(h, data); err != nil {
			return was, err
		}
	}
	if len(still) == was {
		return was, nil
	}
	man.Missing = still // still sorted: a subsequence of a sorted list
	nb, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return was, err
	}
	m.sweep.RLock()
	defer m.sweep.RUnlock()
	if err := writeDurable(m.fsys, m.tmpDir(), m.manifestPath(id), nb); err != nil {
		return was, err
	}
	return len(still), nil
}
