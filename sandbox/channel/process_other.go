//go:build !linux

package channel

import (
	"errors"
	"syscall"
)

// SignalOthers is a no-op off Linux. The sandbox daemon runs as container PID 1
// on Linux; other platforms are not a deployment target.
func SignalOthers(sig syscall.Signal) {}

func listProcesses() ([]Process, error) {
	return nil, errors.New("process list requires linux")
}

func parseSignal(name string) (syscall.Signal, error) {
	return 0, errors.New("signals require linux")
}

func signalPID(pid int, sig syscall.Signal) error {
	return errors.New("signals require linux")
}
