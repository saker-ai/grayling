package web

import "embed"

//go:embed index.html app.js i18n.js style.css
var Assets embed.FS
