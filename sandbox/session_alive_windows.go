//go:build windows

package sandbox

// No process-group introspection on Windows; sessions are unsupported there.
func pgidAlive(pgid int) bool { return false }
