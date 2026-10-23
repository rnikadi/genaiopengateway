// Package ui holds the admin UI: a single-page app served from the
// binary (ADR 0015). The files under static/ are served as they are.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// Files returns the UI's files, rooted at static/.
func Files() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
