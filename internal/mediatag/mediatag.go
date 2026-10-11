// Package mediatag reads an audio file's tags from its head and its tail only (docs/13
// §6, I-164): the web UI's /api/tags and covers, and the search pass's first-chunk read
// (docs/18 §2) share it.
package mediatag

import (
	"io"

	"github.com/dhowden/tag"
)

// Tag reads are bounded: the first Head bytes hold an ID3v2 tag with its picture, the
// last Tail an ID3v1 tag; nothing between is ever streamed.
const (
	Head = 10 << 20
	Tail = 128
)

// Tags is what /api/tags returns (docs/13 §3).
type Tags struct {
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	Album       string `json:"album"`
	AlbumArtist string `json:"album_artist"`
	Track       int    `json:"track"`
	Tracks      int    `json:"tracks"`
	Disc        int    `json:"disc"`
	Year        int    `json:"year"`
	Genre       string `json:"genre"`
	HasCover    bool   `json:"has_cover"`
}

// boundedSeeker exposes only the head and the tail of a file: a read anywhere else is
// an EOF, so a parser that walks the audio frames stops at Head.
type boundedSeeker struct {
	f    io.ReadSeeker
	size int64
	pos  int64
}

func (b *boundedSeeker) Read(p []byte) (int, error) {
	tail := max(b.size-Tail, 0)
	switch {
	case b.pos < Head && b.pos < tail:
		p = p[:min(int64(len(p)), min(Head, tail)-b.pos)]
	case b.pos < tail:
		return 0, io.EOF
	}
	n, err := b.f.Read(p)
	b.pos += int64(n)
	return n, err
}

func (b *boundedSeeker) Seek(off int64, whence int) (int64, error) {
	pos, err := b.f.Seek(off, whence)
	if err == nil {
		b.pos = pos
	}
	return pos, err
}

// Read parses an audio file's tags; a file without any, or with tags the parser
// rejects, is empty tags and no picture, never an error.
func Read(f io.ReadSeeker, size int64) (Tags, *tag.Picture) {
	m, err := tag.ReadFrom(&boundedSeeker{f: f, size: size})
	if err != nil || m == nil {
		return Tags{}, nil
	}
	t := Tags{Title: m.Title(), Artist: m.Artist(), Album: m.Album(), AlbumArtist: m.AlbumArtist(), Year: m.Year(), Genre: m.Genre()}
	t.Track, t.Tracks = m.Track()
	t.Disc, _ = m.Disc()
	pic := m.Picture()
	t.HasCover = pic != nil && len(pic.Data) > 0
	if !t.HasCover {
		pic = nil
	}
	return t, pic
}
