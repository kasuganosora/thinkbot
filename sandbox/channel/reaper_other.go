//go:build !unix

package channel

import (
	"errors"
	"os/exec"
	"syscall"
)

// ExitStatus is the result of waiting on a process the daemon started.
type ExitStatus struct {
	Code   int
	Signal int
	Exited bool
}

// Reaper is unsupported off unix. The sandbox image is Linux.
type Reaper struct{}

func NewReaper() *Reaper { return &Reaper{} }

func (r *Reaper) Close() {}

func (r *Reaper) Start(cmd *exec.Cmd) error {
	return errors.New("channel: process control requires unix")
}

func (r *Reaper) Wait(pid int) (ExitStatus, error) {
	return ExitStatus{}, errors.New("channel: process control requires unix")
}

func SignalGroup(pid int, sig syscall.Signal) error {
	return errors.New("channel: process control requires unix")
}

func (r *Reaper) Forward(sig syscall.Signal) {}
