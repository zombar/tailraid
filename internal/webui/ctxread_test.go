package webui

import (
	"bytes"
	"context"
	"io"
	"testing"
)

// ctxFile is a file whose reads record the context they ran under.
type ctxFile struct {
	*bytes.Reader
	data []byte
	got  []context.Context
}

func (f *ctxFile) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	f.got = append(f.got, ctx)
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

type ctxKey struct{}

// A file served to the browser is read under the request's context, from the offset
// ServeContent sought to, so a client that goes away cuts the fetch (I-314).
// Invariant: I-314
func TestWithContext_ReadsUnderTheRequestContext(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 100)
	f := &ctxFile{Reader: bytes.NewReader(data), data: data}
	ctx := context.WithValue(context.Background(), ctxKey{}, "req")
	rs := withContext(ctx, f)
	if _, err := rs.Seek(123, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(io.LimitReader(rs, 300))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data[123:423]) {
		t.Fatal("bytes differ from the sought range")
	}
	if len(f.got) == 0 {
		t.Fatal("no read went through ReadAtContext")
	}
	for _, c := range f.got {
		if c.Value(ctxKey{}) != "req" {
			t.Fatal("a read ran under another context")
		}
	}
	if pos, _ := rs.Seek(0, io.SeekCurrent); pos != 423 {
		t.Fatalf("offset after reading %d", pos)
	}
}
