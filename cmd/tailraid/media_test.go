package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/scrape"
	"github.com/zombar/tailraid/internal/webui"
)

// The scrape writes as the browser's exclusive upload: the UI header, If-None-Match: *,
// the length, each path segment escaped; a 412 is a name that exists.
// Invariant: I-299
func TestWebWriter_ExclusiveCreate(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.EscapedPath()+" "+r.Header.Get(webui.HeaderUI)+" "+r.Header.Get("If-None-Match")+" "+string(b))
		switch r.URL.Path {
		case "/files/films/Moon (2009)/poster.jpg":
			w.WriteHeader(http.StatusPreconditionFailed)
		case "/files/films/protected/x.nfo":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"put /films/protected/x.nfo: protected"}`))
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()
	w := &webWriter{base: srv.URL, http: srv.Client()}
	ctx := context.Background()
	if err := w.Create(ctx, "/films/Arrival (2016)/movie.nfo", []byte("<movie/>")); err != nil {
		t.Fatal(err)
	}
	if err := w.Create(ctx, "/films/Moon (2009)/poster.jpg", []byte("x")); !errors.Is(err, scrape.ErrExist) {
		t.Errorf("412: %v, want ErrExist", err)
	}
	if err := w.Create(ctx, "/films/protected/x.nfo", []byte("x")); err == nil || errors.Is(err, scrape.ErrExist) || !strings.Contains(err.Error(), "protected") {
		t.Errorf("403: %v", err)
	}
	if want := "PUT /files/films/Arrival%20%282016%29/movie.nfo 1 * <movie/>"; got[0] != want {
		t.Errorf("request %q, want %q", got[0], want)
	}
}
