package web

import (
	"io/fs"
	"net/http"
	"strings"
)

// Handler serves the embedded console and delegates API requests to api.
// Unknown non-API paths fall back to index.html for client-side routes.
func Handler(api http.Handler) http.Handler {
	assets, err := fs.Sub(Assets, ".")
	if err != nil {
		return api
	}
	indexHTML, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return api
	}
	fileServer := http.FileServerFS(assets)
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
		clean := strings.TrimPrefix(r.URL.Path, "/")
		if f, err := assets.Open(clean); err == nil {
			_ = f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}
		serveIndex(w)
	})
}
