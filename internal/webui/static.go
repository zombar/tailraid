package webui

import (
	_ "embed"
	"net/http"
)

//go:embed static/index.html
var indexHTML []byte

//go:embed src/app.css
var appCSS []byte

// indexCSP keeps the page to its own origin: no inline styles or scripts, media and
// images from this node only (docs/13 §3).
const indexCSP = "default-src 'self'; img-src 'self' data:; media-src 'self'; frame-src 'self'"

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Security-Policy", indexCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(indexHTML)
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	var body []byte
	var typ string
	switch r.PathValue("file") {
	case "app.css":
		body, typ = appCSS, "text/css; charset=utf-8"
	default:
		writeErr(w, http.StatusNotFound, "no such asset")
		return
	}
	h := w.Header()
	h.Set("Content-Type", typ)
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(body)
}
