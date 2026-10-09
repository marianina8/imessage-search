// Package web holds the two pages of the app, built into the binary.
package web

import "embed"

// Files are setup.html (the wizard) and index.html (search).
//
//go:embed *.html
var Files embed.FS
