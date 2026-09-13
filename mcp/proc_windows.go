//go:build windows

package mcp

import (
	"os"
	"syscall"
)

func terminateProcess(p *os.Process) {
	if p == nil {
		return
	}
	_ = p.Kill()
}

func processAlive(p *os.Process) bool {
	if p == nil {
		return false
	}
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(p.Pid))
	if err != nil {
		return false
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
