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
}

// Handler returns the /dev/* mux; faults may be nil (no /dev/fault). exit defaults to os.Exit.
func Handler(st *meta.Store, store Store, faults *Faults, log *slog.Logger) http.Handler {
	return handler(st, store, faults, log, os.Exit)
}

func handler(st *meta.Store, store Store, faults *Faults, log *slog.Logger, exit func(int)) http.Handler {
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
	mux.HandleFunc("POST /dev/exit", func(w http.ResponseWriter, _ *http.Request) {
		log.Warn("dev: exit requested")
		w.WriteHeader(http.StatusNoContent)
		time.AfterFunc(100*time.Millisecond, func() { exit(3) })
	})
	return mux
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
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return os.ErrInvalid
	}
	b[len(b)-1] ^= 0xff
	return os.WriteFile(path, b, 0o600)
}
