// Package panel serves the administrator UI: one HTML page plus a script and a
// stylesheet, embedded into the binary.
//
// It is a plain client of the admin API rather than a second implementation of
// the rules: the page calls the same endpoints a curl command would, so the
// panel and the API can never disagree about what an administrator may change.
//
// Everything is same origin and there is no inline script or style, which keeps
// the Content-Security-Policy set by the HTTP middleware strict ('none' by
// default, scripts and styles from 'self' only). Markup inside a channel name
// therefore cannot turn into executable script.
package panel

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web
var web embed.FS

// New returns the handler for GET /admin/.
func New() (http.Handler, error) {
	root, err := fs.Sub(web, "web")
	if err != nil {
		return nil, err
	}
	// The outer router mounts this handler as a subtree of /admin/, so the
	// prefix has to come off before the embedded files are looked up.
	files := http.StripPrefix("/admin/", http.FileServerFS(root))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, root, "index.html")
	})
	mux.HandleFunc("GET /admin/{file}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("file")
		if !strings.HasSuffix(name, ".js") && !strings.HasSuffix(name, ".css") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=300")
		files.ServeHTTP(w, r)
	})
	return mux, nil
}
