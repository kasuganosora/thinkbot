//go:build unix

package sandbox

import "syscall"

// pgidAlive is used after a group kill to detect survivors (for example a
// grandchild that called setsid and escaped the process group). A positive
// result is reported as escapedPids rather than being silently ignored.
func pgidAlive(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	return syscall.Kill(pgid, 0) == nil
}
