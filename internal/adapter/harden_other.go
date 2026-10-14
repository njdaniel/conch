//go:build !linux

package adapter

// hardenProcess does nothing outside Linux; see harden_linux.go.
func hardenProcess() {}
