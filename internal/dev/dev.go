// Package dev serves the /dev/* endpoints of a node whose config has dev.enabled
// (docs/08 §Development endpoints, docs/09 §6). They exist for the soak suite and are
// never registered otherwise.
package dev

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
)

// FSMHash is GET /dev/fsm-hash: the SHA-256 of the encoded FSM state (without the
// applied-index header) and the index it was taken at.
type FSMHash struct {
	Applied uint64 `json:"applied"`
	Hash    string `json:"hash"`
}

// Store is the shard store surface /dev/corrupt needs.
type Store interface {
	Path(k chunk.ShardKey) string
	Has(k chunk.ShardKey) bool
	Disk() (used, free, total uint64, err error)
}

// Handler returns the /dev/* mux; faults may be nil (no /dev/fault). dir is the data
// directory /dev/fill consumes. exit defaults to os.Exit.
func Handler(st *meta.Store, store Store, dir string, faults *Faults, log *slog.Logger) http.Handler {
	return handler(st, store, dir, faults, log, os.Exit)
}

func handler(st *meta.Store, store Store, dir string, faults *Faults, log *slog.Logger, exit func(int)) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	mux := http.NewServeMux()
	if faults != nil {
		mux.HandleFunc("GET /dev/fault", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"drop": faults.List()})
		})
		mux.HandleFunc("POST /dev/fault", func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Drop []string `json:"drop"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			faults.Set(req.Drop)
			log.Warn("dev: fault rules set", "drop", faults.List())
			w.WriteHeader(http.StatusNoContent)
		})
	}
	mux.HandleFunc("GET /dev/fsm-hash", func(w http.ResponseWriter, _ *http.Request) {
		h, err := Hash(st)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h)
	})
	mux.HandleFunc("POST /dev/corrupt", func(w http.ResponseWriter, r *http.Request) {
		key := chunk.ShardKey(r.URL.Query().Get("key"))
		if key == "" || !store.Has(key) {
			http.Error(w, "unknown shard key", http.StatusNotFound)
			return
		}
		if err := Corrupt(store.Path(key)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Warn("dev: shard corrupted on request", "key", string(key))
		w.WriteHeader(http.StatusNoContent)
	})
	// the stack of every goroutine, as a crash dump would print it: a hung node is
	// diagnosed without killing it (docs/12 §3)
	mux.HandleFunc("GET /dev/goroutines", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_ = pprof.Lookup("goroutine").WriteTo(w, 2)
	})
	// the data disk as the budget sees it, and a scratch file that consumes it (docs/01
	// §Node budget): the soak suite's way to move a node's free space
	mux.HandleFunc("GET /dev/disk", func(w http.ResponseWriter, _ *http.Request) {
		used, free, total, err := store.Disk()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]uint64{"used": used, "free": free, "total": total})
	})
	mux.HandleFunc("POST /dev/fill", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.ParseUint(r.URL.Query().Get("bytes"), 10, 64)
		if err != nil {
			http.Error(w, "bytes: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := Fill(dir, n); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Warn("dev: scratch fill set", "bytes", n)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /dev/exit", func(w http.ResponseWriter, _ *http.Request) {
		log.Warn("dev: exit requested")
		w.WriteHeader(http.StatusNoContent)
		time.AfterFunc(100*time.Millisecond, func() { exit(3) })
	})
	return mux
}

// Fill writes (or removes, for 0) the scratch file dir/dev-fill of n bytes, really
// allocated so the filesystem's free space moves.
func Fill(dir string, n uint64) error {
	p := filepath.Join(dir, "dev-fill")
	if n == 0 {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	f, err := os.Create(p) //nolint:gosec // the fill file lives in this node's data directory
	if err != nil {
		return err
	}
	block := make([]byte, 1<<20)
	for left := n; left > 0; {
		w := min(left, uint64(len(block)))
		if _, err := f.Write(block[:w]); err != nil {
			_ = f.Close()
			return err
		}
		left -= w
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Hash computes the FSMHash of st's current state.
func Hash(st *meta.Store) (FSMHash, error) {
	snap, err := st.Snapshot()
	if err != nil {
		return FSMHash{}, err
	}
	state := snap.(interface{ State() *meta.State }).State()
	var buf bytes.Buffer
	if err := meta.EncodeState(&buf, state); err != nil {
		return FSMHash{}, err
	}
	sum := sha256.Sum256(buf.Bytes()[13:]) // skip magic, version and applied index
	return FSMHash{Applied: st.Applied(), Hash: hex.EncodeToString(sum[:])}, nil
}

// Corrupt flips the last payload byte of a shard file in place: the header hash no longer
// matches, so the store reports ErrCorrupt and quarantines it on the next read.
func Corrupt(path string) error {
	b, err := os.ReadFile(path) //nolint:gosec // a shard inside this node's data directory
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return os.ErrInvalid
	}
	b[len(b)-1] ^= 0xff
	return os.WriteFile(path, b, 0o600) //nolint:gosec // path is a shard inside this node's own data directory
}
