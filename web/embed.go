package web

import "embed"

//go:embed index.html app.js approvals.js exp_workbench.js flag_editor.js segment_editor.js i18n.js style.css
var Assets embed.FS
