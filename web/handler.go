package web

import (
	"io/fs"
	"net/http"
	"strings"
)

// immutableCache is the Cache-Control value for Vite-hashed assets under
// /assets/: content-addressed file names may be cached forever.
const immutableCache = "public, max-age=31536000, immutable"

// Handler serves the embedded React console (StaticFS) and delegates API
// requests to api. Unknown non-API paths fall back to the React index.html for
// client-side (hash) routes.
func Handler(api http.Handler) http.Handler {
	static, err := fs.Sub(StaticFS, "static")
	if err != nil {
		return api
	}
	indexHTML, err := fs.ReadFile(static, "index.html")
	if err != nil {
		return api
	}
	staticFiles := http.FileServerFS(static)
	serveIndex := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/v1/") {
			api.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			serveIndex(w)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if strings.HasPrefix(name, "assets/") {
			if _, err := static.Open(name); err == nil {
				// Vite emits content-hashed file names: cache them forever.
				w.Header().Set("Cache-Control", immutableCache)
				staticFiles.ServeHTTP(w, r)
				return
			}
			serveIndex(w)
			return
		}
		if _, err := static.Open(name); err == nil {
			staticFiles.ServeHTTP(w, r)
			return
		}
		serveIndex(w)
	})
}
