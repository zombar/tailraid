package webui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // registers the decoder
	"image/jpeg"
	_ "image/png" // registers the decoder
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers the decoder

	"github.com/zombar/tailraid/internal/meta"
)

// Thumbnails (docs/13 §5, I-160) live in the volume at
// /.tailraid/thumbs/<ino>-<gen>-<version>.jpg, at most ThumbWidth wide; a frame the
// browser grabbed carries its recipe, <ino>-<gen>-<version>.f<FrameRecipe>.jpg (I-216).
const (
	ThumbDir   = "/" + meta.ReservedDir + "/thumbs"
	ThumbWidth = 320
	// ThumbPutMax bounds a PUT /api/thumb body (docs/08 I-116).
	ThumbPutMax = 256 << 10
	// ThumbPixels bounds what the node decodes itself: a picture above it is 415.
	ThumbPixels = 64 << 20
	// ThumbDecoders bounds the decodes in flight.
	ThumbDecoders = 2
	thumbQuality  = 80
	// FrameRecipe is the browser's frame choice for a video (docs/13 §5, thumbs.ts
	// RECIPE): 2 waits for a presented frame and skips dark and flat ones. A frame of
	// another recipe is not this file's thumbnail, so bumping it re-grabs every video.
	FrameRecipe = 2
)

// thumbKey is the file name of in's thumbnail: the node makes an image's or an audio
// file's itself, anything else's is a frame the browser grabbed under FrameRecipe.
func thumbKey(in meta.Inode) string {
	if isImage(in.Name) || isAudio(in.Name) {
		return fmt.Sprintf("%d-%d-%d.jpg", in.Ino, in.Gen, in.Version)
	}
	return fmt.Sprintf("%d-%d-%d.f%d.jpg", in.Ino, in.Gen, in.Version, FrameRecipe)
}

// parseThumbKey reads a key back, with or without a frame recipe; ok is false for a
// foreign name.
func parseThumbKey(name string) (ino meta.Ino, gen uint32, version uint64, ok bool) {
	base, found := strings.CutSuffix(name, ".jpg")
	if !found {
		return 0, 0, 0, false
	}
	if i := strings.LastIndex(base, ".f"); i >= 0 {
		if _, err := strconv.ParseUint(base[i+2:], 10, 32); err != nil {
			return 0, 0, 0, false
		}
		base = base[:i]
	}
	parts := strings.Split(base, "-")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	i, err1 := strconv.ParseUint(parts[0], 10, 64)
	g, err2 := strconv.ParseUint(parts[1], 10, 32)
	v, err3 := strconv.ParseUint(parts[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, 0, false
	}
	return meta.Ino(i), uint32(g), v, true
}

// hasThumb reports whether in's thumbnail is stored.
func (s *Server) hasThumb(in meta.Inode) bool {
	_, err := s.o.Store.Resolve([]string{meta.ReservedDir, "thumbs", thumbKey(in)})
	return err == nil
}

// thumbGet serves a file's thumbnail: stored, else made now for an image or an audio
// file with a picture; 404 for a video without one, or anything else.
func (s *Server) thumbGet(w http.ResponseWriter, r *http.Request) {
	t, err := s.resolveTarget(r, r.URL.Query().Get("path"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if t.in.Type == meta.TDir { // a folder's tile is its cover track's picture, resolved here and remembered (I-165)
		name := s.coverOf(t.in)
		if name == "" {
			writeErr(w, http.StatusNotFound, "no thumbnail")
			return
		}
		if t, err = s.resolveTarget(r, path.Join(r.URL.Query().Get("path"), name)); err != nil {
			s.fail(w, err)
			return
		}
	}
	if t.in.Type != meta.TFile {
		writeErr(w, http.StatusBadRequest, "thumb "+t.canonical+": "+errNotFile.Error())
		return
	}
	key := thumbKey(t.in)
	stored := path.Join(ThumbDir, key)
	if s.hasThumb(t.in) {
		s.serveThumb(w, r, stored, key)
		return
	}
	var data []byte
	switch {
	case isImage(t.in.Name):
		data, err = s.makeImageThumb(r.Context(), t)
	case isAudio(t.in.Name):
		data, err = s.makeAudioThumb(r.Context(), t)
	default:
		err = noThumb(t.canonical)
	}
	if err != nil {
		switch {
		case errors.Is(err, errNoThumb):
			writeErr(w, http.StatusNotFound, "no thumbnail")
		case errors.Is(err, errTooLarge):
			writeErr(w, http.StatusUnsupportedMediaType, err.Error())
		case errors.Is(err, image.ErrFormat):
			writeErr(w, http.StatusUnsupportedMediaType, "thumb "+t.canonical+": "+err.Error())
		default:
			s.fail(w, err)
		}
		return
	}
	if err := s.storeThumb(key, data); err != nil {
		s.o.Logger.Warn("thumbnail not stored", "key", key, "err", err)
	}
	thumbHeaders(w, key)
	http.ServeContent(w, r, key, time.Time{}, bytes.NewReader(data))
}

func thumbHeaders(w http.ResponseWriter, key string) {
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("Cache-Control", "private, max-age=86400")
	h.Set("ETag", `"`+key+`"`)
	h.Set("X-Content-Type-Options", "nosniff")
}

func (s *Server) serveThumb(w http.ResponseWriter, r *http.Request, stored, key string) {
	f, err := s.o.FS.Open(stored)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer f.Close()
	thumbHeaders(w, key)
	http.ServeContent(w, r, key, time.Time{}, f)
}

var errTooLarge = errors.New("the picture is above the pixel bound")

// makeImageThumb decodes an image the node can read and scales it (two at a time).
func (s *Server) makeImageThumb(ctx context.Context, t target) ([]byte, error) {
	f, err := s.open(t)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return s.scale(ctx, f)
}

// scale decodes r (header first, so the pixel bound is checked before any pixel) and
// encodes a JPEG at most ThumbWidth wide.
func (s *Server) scale(ctx context.Context, r io.ReadSeeker) ([]byte, error) {
	select {
	case s.decoders <- struct{}{}:
		defer func() { <-s.decoders }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	cfg, _, err := image.DecodeConfig(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", image.ErrFormat, err)
	}
	if int64(cfg.Width)*int64(cfg.Height) > ThumbPixels {
		return nil, fmt.Errorf("%dx%d: %w", cfg.Width, cfg.Height, errTooLarge)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", image.ErrFormat, err)
	}
	return encodeThumb(img)
}

// encodeThumb scales img to ThumbWidth when wider and encodes it as JPEG.
func encodeThumb(img image.Image) ([]byte, error) {
	b := img.Bounds()
	if b.Dx() > ThumbWidth {
		h := max(1, b.Dy()*ThumbWidth/max(b.Dx(), 1))
		dst := image.NewRGBA(image.Rect(0, 0, ThumbWidth, h))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
		img = dst
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: thumbQuality}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// makeAudioThumb is the file's embedded picture, else a cover.*/folder.* sibling, in
// that order of names (I-165), scaled like an image.
func (s *Server) makeAudioThumb(ctx context.Context, t target) ([]byte, error) {
	f, err := s.open(t)
	if err != nil {
		return nil, err
	}
	_, pic := readTags(f, int64(t.in.Size))
	_ = f.Close()
	if pic != nil {
		data, err := s.scale(ctx, bytes.NewReader(pic.Data))
		if err == nil {
			return data, nil
		}
	}
	dir := t.parts[:len(t.parts)-1]
	for _, name := range coverNames {
		sib := target{parts: append(append([]string{}, dir...), name), root: t.root, trashed: t.trashed}
		sib.canonical = "/" + strings.Join(sib.parts, "/")
		var err error
		if t.trashed {
			sib.in, err = s.o.Store.ResolveFrom(t.root.Ino, sib.parts)
		} else {
			sib.in, err = s.o.Store.Resolve(sib.parts)
		}
		if err != nil || sib.in.Type != meta.TFile {
			continue
		}
		if data, err := s.makeImageThumb(ctx, sib); err == nil {
			return data, nil
		}
	}
	return nil, noThumb(t.canonical)
}

// storeThumb writes data under ThumbDir the way an upload lands: a temp beside the
// target and one Rename (docs/13 §4).
func (s *Server) storeThumb(key string, data []byte) error {
	if err := s.o.FS.MkdirAll(ThumbDir, 0o755); err != nil {
		return err
	}
	temp := path.Join(ThumbDir, "."+key+meta.UploadSuffix)
	f, err := s.o.FS.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = s.o.FS.Remove(temp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = s.o.FS.Remove(temp)
		return err
	}
	return s.o.FS.Rename(temp, path.Join(ThumbDir, key))
}

// thumbPut stores the frame the browser grabbed from a video (docs/13 §5): a JPEG of
// at most ThumbPutMax bytes for the file's current Version.
func (s *Server) thumbPut(w http.ResponseWriter, r *http.Request) {
	parts, canonical := cleanPath(r.URL.Query().Get("path"))
	if reserved(parts) || len(parts) == 0 {
		writeErr(w, http.StatusBadRequest, "thumb "+canonical+": not a user file")
		return
	}
	in, err := s.o.Store.Resolve(parts)
	if err != nil {
		s.fail(w, err)
		return
	}
	if in.Type != meta.TFile {
		writeErr(w, http.StatusBadRequest, "thumb "+canonical+": "+errNotFile.Error())
		return
	}
	version, err := strconv.ParseUint(r.URL.Query().Get("version"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "thumb "+canonical+": version is required")
		return
	}
	if version != in.Version {
		writeErr(w, http.StatusConflict, fmt.Sprintf("thumb %s: the file is at version %d, not %d", canonical, in.Version, version))
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, ThumbPutMax))
	if err != nil {
		if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("thumb %s: body too large: at most %d bytes", canonical, ThumbPutMax))
		} else {
			writeErr(w, http.StatusBadRequest, "thumb "+canonical+": "+err.Error())
		}
		return
	}
	if len(data) < 3 || data[0] != 0xFF || data[1] != 0xD8 || data[2] != 0xFF {
		writeErr(w, http.StatusUnsupportedMediaType, "thumb "+canonical+": not a JPEG")
		return
	}
	if err := s.storeThumb(thumbKey(in), data); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// SweepThumbs removes every thumbnail whose (ino, gen, version) no longer names a file,
// live or in the trash (I-160), and every one that is not the file's current key: a
// video's frame of another recipe or under the plain key (I-216). The hourly sweep runs
// it beside SweepUploads.
func SweepThumbs(ctx context.Context, fs Remover, st *meta.Store) (int, error) {
	dir, err := st.Resolve([]string{meta.ReservedDir, "thumbs"})
	if err != nil {
		return 0, nil //nolint:nilerr // no thumbnails yet
	}
	kids, err := st.ReadDir(dir.Ino)
	if err != nil {
		return 0, nil //nolint:nilerr // removed meanwhile
	}
	removed := 0
	for i := range kids {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		k := &kids[i]
		if k.Type != meta.TFile || IsUploadTemp(k.Name) {
			continue
		}
		ino, _, _, ok := parseThumbKey(k.Name)
		if !ok {
			continue // not a thumbnail of ours: left alone
		}
		if in, found := st.GetInodeAny(ino); found && thumbKey(in) == k.Name {
			continue // the current thumbnail of a file, live or in the trash
		}
		if err := fs.Remove(path.Join(ThumbDir, k.Name)); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
