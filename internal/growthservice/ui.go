package growthservice

import (
	"embed"
	"net/http"
)

//go:embed ui/*
var pages embed.FS

func (s *Service) page(w http.ResponseWriter, r *http.Request) {
	name := "index.html"
	kind := "text/html; charset=utf-8"
	if r.URL.Path == "/app.js" {
		name = "app.js"
		kind = "text/javascript; charset=utf-8"
	}
	if r.URL.Path == "/style.css" {
		name = "style.css"
		kind = "text/css; charset=utf-8"
	}
	b, err := pages.ReadFile("ui/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", kind)
	_, _ = w.Write(b)
}
