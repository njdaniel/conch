//go:build !conchweb

// Package web holds the web client built into the conchd binary. This build
// has none: see embed.go for the one built with -tags conchweb.
package web

import "io/fs"

// Files reports that no client is embedded.
func Files() (fs.FS, bool) { return nil, false }
