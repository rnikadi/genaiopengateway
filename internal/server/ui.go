package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/rnikadi/genaiopengateway/ui"
)

// uiFile is one file of the admin UI, read once at startup.
type uiFile struct {
	body        []byte
	contentType string
	etag        string
}

func loadUI() map[string]*uiFile {
	out := map[string]*uiFile{}
	_ = fs.WalkDir(ui.Files(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(ui.Files(), p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		ct := mime.TypeByExtension(path.Ext(p))
		if ct == "" {
			ct = "application/octet-stream"
		}
		out[p] = &uiFile{body: b, contentType: ct, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		return nil
	})
	return out
}

// uiHeaders are set on every admin UI response (docs/admin-ui.md#serving-the-ui).
func uiHeaders(h http.Header) {
	// The playground shows generated images, inline or at the provider's
	// URL, and plays generated speech from a blob.
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob: https:; media-src 'self' blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
}

// serveUI serves the UI's files. Browsers revalidate each file by ETag,
// so a new gateway version is picked up on the next load.
func (s *Server) serveUI(w http.ResponseWriter, r *http.Request) {
	if !s.rt.Load().cfg.UIEnabled() {
		http.NotFound(w, r)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/ui/")
	if p == "" {
		p = "index.html"
	}
	f := s.uiFiles[p]
	if f == nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	uiHeaders(h)
	h.Set("Content-Type", f.contentType)
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", f.etag)
	if r.Header.Get("If-None-Match") == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(f.body)
}

// uiRoutes registers the UI and operator sign-in.
func (s *Server) uiRoutes(mux *http.ServeMux) {
	s.uiFiles = loadUI()
	mux.Handle("GET /ui", http.RedirectHandler("/ui/", http.StatusMovedPermanently))
	mux.HandleFunc("GET /ui/", s.serveUI)
	auth := func(fn http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if s.store == nil || !s.rt.Load().cfg.UIEnabled() {
				http.NotFound(w, r)
				return
			}
			uiHeaders(w.Header())
			w.Header().Set("Cache-Control", "no-store")
			fn(w, r)
		}
	}
	mux.HandleFunc("GET /ui/auth/config", auth(s.authConfig))
	mux.HandleFunc("GET /ui/auth/login", auth(s.login))
	mux.HandleFunc("GET /ui/auth/callback", auth(s.callback))
	mux.HandleFunc("POST /ui/auth/token", auth(s.tokenLogin))
	mux.HandleFunc("POST /ui/auth/logout", auth(s.logout))
}
