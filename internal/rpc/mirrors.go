package rpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/mirror"
)

// MaxTree bounds a tree reply: a directory of about 100k entries (docs/16 §8).
const MaxTree = 64 << 20

// mirrorGet serves GET /v1/mirrors/{idx}/{kind}/{hash}: a snapshot's tree or chunk object
// from a mirror open on this host, to a member, on the GET slots (docs/16 §6). The host
// verifies what it reads before it serves it; a mirror not open here is a 404 marked
// with HeaderDisk, as a closed disk is.
func (s *Server) mirrorGet(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.ParseUint(r.PathValue("idx"), 10, 16)
	h, ok := parseHashHex(r.PathValue("hash"))
	kind := r.PathValue("kind")
	if err != nil || !ok || (kind != "objects" && kind != "trees") {
		writeErr(w, http.StatusBadRequest, "bad mirror path")
		return
	}
	var m *mirror.Mirror
	if s.o.Mirror != nil {
		m, ok = s.o.Mirror(meta.NodeIdx(idx))
	}
	if !ok || m == nil {
		w.Header().Set(HeaderDisk, "closed")
		writeErr(w, http.StatusNotFound, "mirror "+r.PathValue("idx")+" is not open on this host")
		return
	}
	if background(r) {
		if !acquire(r.Context(), s.bgGetSem) {
			writeErr(w, http.StatusServiceUnavailable, "cancelled while queued")
			return
		}
		defer func() { <-s.bgGetSem }()
	}
	if !acquire(r.Context(), s.getSem) {
		writeErr(w, http.StatusServiceUnavailable, "cancelled while queued")
		return
	}
	defer func() { <-s.getSem }()
	var body []byte
	if kind == "objects" {
		body, err = m.Object(h)
	} else {
		var t mirror.Tree
		if t, err = m.Tree(h); err == nil {
			body, err = mirror.EncodeTree(t) // canonical: the bytes the hash names
		}
	}
	switch {
	case errors.Is(err, mirror.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, mirror.ErrCorrupt):
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.o.BodyTimeout)) // I-97
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

func parseHashHex(s string) (meta.Hash, bool) {
	var h meta.Hash
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(h) {
		return h, false
	}
	copy(h[:], b)
	return h, true
}

// MirrorObject fetches chunk h from mirror row idx on its host and checks it hashes to h.
func (c *Client) MirrorObject(ctx context.Context, idx meta.NodeIdx, h meta.Hash) ([]byte, error) {
	b, err := c.mirrorGet(ctx, idx, "objects", h, chunk.MaxPayload)
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(b) != h {
		return nil, fmt.Errorf("%w: object %x from mirror %d", mirror.ErrCorrupt, h[:4], idx)
	}
	return b, nil
}

// MirrorTree fetches tree h from mirror row idx on its host, checked against h.
func (c *Client) MirrorTree(ctx context.Context, idx meta.NodeIdx, h meta.Hash) (mirror.Tree, error) {
	b, err := c.mirrorGet(ctx, idx, "trees", h, MaxTree)
	if err != nil {
		return mirror.Tree{}, err
	}
	if mirror.TreeHash(b) != h {
		return mirror.Tree{}, fmt.Errorf("%w: tree %x from mirror %d", mirror.ErrCorrupt, h[:4], idx)
	}
	t, err := mirror.DecodeTree(b)
	if err != nil {
		return mirror.Tree{}, fmt.Errorf("%w: tree %x: %w", mirror.ErrCorrupt, h[:4], err)
	}
	return t, nil
}

// mirrorGet is one GET of a mirror's object or tree: the body bounded by limit and, once
// the headers are in, by callTimeout of its announced length (I-171).
func (c *Client) mirrorGet(ctx context.Context, idx meta.NodeIdx, kind string, h meta.Hash, limit int64) ([]byte, error) {
	addr, _, ok := c.resolve(idx)
	if !ok {
		return nil, fmt.Errorf("%w: mirror %d has no address", ErrUnavailable, idx)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	path := mirrorPrefix + strconv.Itoa(int(idx)) + "/" + kind + "/" + hex.EncodeToString(h[:])
	resp, err := c.do(ctx, http.MethodGet, addr, path, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := errFromStatus(resp)
		switch {
		case errors.Is(err, chunk.ErrNotFound):
			return nil, fmt.Errorf("%w: %s %x on mirror %d", mirror.ErrNotFound, kind, h[:4], idx)
		case errors.Is(err, chunk.ErrCorrupt):
			return nil, fmt.Errorf("%w: %s %x on mirror %d", mirror.ErrCorrupt, kind, h[:4], idx)
		}
		return nil, err
	}
	n := resp.ContentLength
	if n < 0 || n > limit {
		return nil, fmt.Errorf("%w: a %s reply of %d bytes", ErrTooLarge, kind, n)
	}
	t := c.clk.NewTimer(callTimeout(n))
	defer t.Stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-t.C():
			cancel()
		case <-done:
		}
	}()
	b := make([]byte, n)
	if _, err := io.ReadFull(resp.Body, b); err != nil {
		return nil, fmt.Errorf("%w: GET %s body: %w", ErrUnavailable, addr, err)
	}
	return b, nil
}
