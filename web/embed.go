// Package web holds the page the hub serves. No frameworks, no external requests.
package web

import "embed"

// FS is the page: index.html, app.js, style.css and the bundled font subset.
//
//go:embed index.html app.js style.css fonts
var FS embed.FS
