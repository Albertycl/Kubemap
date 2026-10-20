// Package web embeds the built portal UI (web/dist, made by "make web") into
// kubemap-portal and serves it.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// dist holds the Vite build. "all:" keeps web/dist/.gitkeep, so the package
// compiles before the UI is built.
//
//go:embed all:dist
var dist embed.FS

// contentSecurityPolicy allows only the portal's own scripts, styles and API.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

const notBuilt = `<!doctype html><meta charset="utf-8"><title>Kubemap</title>
<p>The Kubemap web UI is not built into this binary. Run <code>make web</code>, then build the portal again.</p>
<p>The API is available under <code>/api/</code>.</p>`

// Routes serves the UI at / and its files under /assets/. Asset names carry
// a content hash, so they are cached for a year; index.html is never cached.
func Routes(mux *http.ServeMux) {
	files, _ := fs.Sub(dist, "dist")
	routes(mux, files)
}

func routes(mux *http.ServeMux, files fs.FS) {
	index, err := fs.ReadFile(files, "index.html")
	built := err == nil

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		if !built {
			w.Write([]byte(notBuilt))
			return
		}
		w.Write(index)
	})
	static := http.FileServerFS(files)
	mux.HandleFunc("GET /assets/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if st, err := fs.Stat(files, name); err != nil || st.IsDir() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		static.ServeHTTP(w, r)
	})
}
