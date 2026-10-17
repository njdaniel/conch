//go:build conchweb

// Package web holds the web client built into the conchd binary. It is
// there only when conchd is built with -tags conchweb (make
// build-web-embedded), which needs internal/web/dist to hold a build
// (make web-embed). Without the tag the package embeds nothing and a plain
// go build needs neither Node nor web/dist.
package web

import (
	"embed"
	"io/fs"
)

// The tag says "the client is in here": with dist empty or absent, this
// directive makes the compile fail, which is intended.
//
//go:embed all:dist
var dist embed.FS

// Files returns the embedded build, rooted where index.html is, and true.
func Files() (fs.FS, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Unreachable: "dist" is a valid path and the embed above succeeded.
		return nil, false
	}
	return sub, true
}
