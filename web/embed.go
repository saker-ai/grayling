package web

import "embed"

// StaticFS holds the built React console (Vite output in web/static, committed
// to git). A clean checkout without a frontend build carries the placeholder
// produced by `make test-static`.
//
//go:embed all:static
var StaticFS embed.FS
