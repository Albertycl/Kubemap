package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func serve(t *testing.T, files fstest.MapFS, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	routes(mux, files)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestBuiltUI(t *testing.T) {
	files := fstest.MapFS{
		"index.html":           {Data: []byte(`<div id="root"></div>`)},
		"assets/index-abc.js":  {Data: []byte("console.log(1)")},
		"assets/index-abc.css": {Data: []byte("body{}")},
	}
	w := serve(t, files, "/")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `id="root"`) {
		t.Fatalf("/: %d %s", w.Code, w.Body)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP %q", csp)
	}
	if w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatal("index.html must not be cached")
	}
	w = serve(t, files, "/assets/index-abc.js")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("asset: %d %v", w.Code, w.Header())
	}
	for _, p := range []string{"/assets/", "/assets/missing.js", "/assets/../index.html", "/nothing"} {
		if w := serve(t, files, p); w.Code == 200 {
			t.Errorf("%s: 200, want an error", p)
		}
	}
}

func TestUINotBuilt(t *testing.T) {
	w := serve(t, fstest.MapFS{".gitkeep": {}}, "/")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "make web") {
		t.Fatalf("/: %d %s", w.Code, w.Body)
	}
}

// TestEmbeddedFS checks the real embed compiles and serves either the build
// or the "not built" page.
func TestEmbeddedFS(t *testing.T) {
	mux := http.NewServeMux()
	Routes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 || !(strings.Contains(w.Body.String(), `id="root"`) || strings.Contains(w.Body.String(), "make web")) {
		t.Fatalf("/: %d %s", w.Code, w.Body)
	}
}
