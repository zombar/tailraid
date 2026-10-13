// Package xdr is go-nfs's XDR reading and writing (tailraid, I-244): the helpers of
// go-nfs-client/nfs/xdr, with every length a request names bounded by the bytes the
// request holds. A request is one frame in memory (MaxFrame), so a length past what is
// left of it is garbage, and believing it allocated up to 4 GiB for a 44-byte call.
package xdr

import (
	"errors"
	"fmt"
	"io"

	xdr "github.com/rasky/go-xdr/xdr2"
)

// ErrGarbage wraps every failure to decode a request: a length past the request, a short
// body, a value out of range. A server answers it GARBAGE_ARGS (RFC 5531 §9).
var ErrGarbage = errors.New("xdr: arguments do not decode")

// lener is a reader that knows how many bytes are left: a request body (bytes.Reader).
type lener interface{ Len() int }

// limit is the most bytes one element read from r may claim: what r holds, or the
// largest frame a server accepts when r cannot say.
func limit(r io.Reader) uint {
	if l, ok := r.(lener); ok {
		return uint(l.Len())
	}
	return maxUnknown
}

// maxUnknown bounds an element of a reader that cannot say its length (the stream a
// frame header is read from); it is the largest frame go-nfs accepts.
const maxUnknown = 1<<20 + 8<<10

// Read decodes val from r, refusing any opaque, string or array longer than r holds.
func Read(r io.Reader, val any) error {
	if _, err := xdr.UnmarshalLimited(r, val, max(limit(r), 1)); err != nil {
		return fmt.Errorf("%w: %w", ErrGarbage, err)
	}
	return nil
}

// ReadUint32 decodes one unsigned integer.
func ReadUint32(r io.Reader) (uint32, error) {
	var n uint32
	err := Read(r, &n)
	return n, err
}

// ReadOpaque decodes a variable-length opaque, refusing a length past what r holds.
func ReadOpaque(r io.Reader) ([]byte, error) {
	n, err := ReadUint32(r)
	if err != nil {
		return nil, err
	}
	if uint(n) > limit(r) {
		return nil, fmt.Errorf("%w: an opaque of %d bytes in a request holding %d", ErrGarbage, n, limit(r))
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrGarbage, err)
	}
	return buf, nil
}

// Write encodes val to w.
func Write(w io.Writer, val any) error {
	_, err := xdr.Marshal(w, val)
	return err
}
