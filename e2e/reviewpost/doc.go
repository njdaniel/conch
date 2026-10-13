// Package reviewpost holds the end-to-end tests of ops/principal-review.sh
// (issue #26): the script that runs the principal-review loop from a systemd
// user timer and posts a summary of its report into Conch.
//
// The tests build conchd and conch, start a real conchd on a loopback port
// with a temporary data directory, and run the script against it with the
// built conch first on PATH. Nothing outside loopback is touched: `run` is
// driven with a stand-in for the claude binary, a stub gh, and a bare
// repository on disk as its origin; `install` and `status` get a stub
// systemctl and a temporary HOME. Every script run is given stubs for claude,
// gh and systemctl that fail loudly, so a test that forgets one cannot reach
// the real thing.
//
// The script needs flock(1) and systemd, so the tests are Linux only.
package reviewpost
