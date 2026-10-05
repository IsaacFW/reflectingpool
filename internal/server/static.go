package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// screens are the paths that belong to the interface. Each serves the same
// page, and the script in it reads the path; this is what lets a reload or a
// bookmark of /space?path=... work.
var screens = []string{"overview", "space", "find", "review", "annotations", "quarantine", "storage", "settings"}

// shellCSP is the policy for the one HTML page. Scripts and styles come from
// this server only, with nothing inline, so text taken from file names or
// notes cannot become code. Images, media and frames are our own preview
// route; blob: is for previews built in the page.
const shellCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' blob:; " +
	"media-src 'self'; frame-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

var staticTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".svg":  "image/svg+xml",
}

func (s *Server) shell(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", shellCSP)
	s.serveFile(w, r, "index.html")
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, strings.TrimPrefix(r.URL.Path, "/"))
}

// serveFile sends one file of the interface. The browser keeps a copy but
// asks each time whether it is still current, so a new version of the
// program is picked up on the next load without anyone clearing a cache.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	ctype, known := staticTypes[path.Ext(name)]
	data, err := fs.ReadFile(s.web, name)
	if err != nil || !known {
		http.NotFound(w, r)
		return
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:12]) + `"`
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(data)
}
