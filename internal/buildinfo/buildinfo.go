// Package buildinfo names the build a binary came from, for conchd and conch.
//
// Release builds and `make build` stamp a version with
// -ldflags "-X main.version=...". A binary built without it still says which
// commit it is, from the VCS data the Go toolchain embeds, so a tester can
// always report a build.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// Unknown is what a binary says when nothing was stamped and the toolchain
// recorded nothing either: go run, go test, or a build outside a checkout.
const Unknown = "v0.0.0-dev"

// shortRev is how much of a commit hash is shown; git's own default is 7, and
// 12 stays unambiguous for longer.
const shortRev = 12

// Version is the version to report. stamped is the value the linker put in
// the binary's version variable, empty when there is none.
func Version(stamped string) string {
	return resolve(stamped, debug.ReadBuildInfo)
}

// resolve is Version with the build-info reader passed in, so a test can
// stand in for a binary built in a checkout.
func resolve(stamped string, read func() (*debug.BuildInfo, bool)) string {
	if s := strings.TrimSpace(stamped); s != "" {
		return s
	}
	info, ok := read()
	if !ok {
		return Unknown
	}
	// go install pkg@v1.2.3 records the module version; "(devel)" is what a
	// build from a checkout records, which says nothing.
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return Unknown
	}
	if len(rev) > shortRev {
		rev = rev[:shortRev]
	}
	v := "dev-" + rev
	if dirty {
		v += "-dirty"
	}
	return v
}
