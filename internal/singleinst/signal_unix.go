//go:build unix

package singleinst

import "syscall"

func terminatePID(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}
