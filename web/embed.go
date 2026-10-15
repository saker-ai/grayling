package web

import "embed"

//go:embed index.html app.js approvals.js exp_workbench.js flag_editor.js i18n.js presets.js segment_editor.js style.css
var Assets embed.FS
