//go:build linux

package channel

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// ExitStatus is the result of waiting on a process the daemon started.
type ExitStatus struct {
	Code   int
	Signal int
	Exited bool
}

type savedExit struct {
	st ExitStatus
	at time.Time
}

// Reaper is the PID 1 / subreaper wait loop.
//
// Go's os/exec.Wait and a global Wait4 race, so the daemon never Wait()s a
// Cmd it started here. This loop is the only waiter: it collects exit statuses
// for tracked children and reaps everyone else so zombies do not accumulate
// when a shell double-forks or a grandchild is reparented.
type Reaper struct {
	mu      sync.Mutex
	waiters map[int]chan ExitStatus
	exited  map[int]savedExit
	groups  map[int]struct{}
	once    sync.Once
	stop    chan struct{}
	stopped chan struct{}
	wake    chan struct{}
}

// NewReaper starts the wait loop and marks this process as a child subreaper
// so grandchildren of commands we spawn are reparented here even when we are
// not yet PID 1 (tests, and a daemon that was accidentally wrapped).
func NewReaper() *Reaper {
	r := &Reaper{
		waiters: map[int]chan ExitStatus{},
		exited:  map[int]savedExit{},
		groups:  map[int]struct{}{},
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
		wake:    make(chan struct{}, 1),
	}
	enableChildSubreaper()
	go r.loop()
	return r
}

func (r *Reaper) loop() {
	defer close(r.stopped)
	sigs := make(chan os.Signal, 32)
	signal.Notify(sigs, syscall.SIGCHLD)
	defer signal.Stop(sigs)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		r.reap()
		select {
		case <-r.stop:
			r.reap()
			return
		case <-sigs:
		case <-r.wake:
		case <-tick.C:
		}
	}
}

func (r *Reaper) kick() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Close stops the wait loop. It is safe to call more than once.
func (r *Reaper) Close() {
	r.once.Do(func() { close(r.stop) })
	<-r.stopped
}

// Start runs cmd in its own process group and tracks the leader.
func (r *Reaper) Start(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		return err
	}
	r.mu.Lock()
	r.groups[cmd.Process.Pid] = struct{}{}
	r.mu.Unlock()
	r.kick()
	return nil
}

// Wait blocks until pid has been reaped.
func (r *Reaper) Wait(pid int) (ExitStatus, error) {
	r.mu.Lock()
	if st, ok := r.exited[pid]; ok {
		delete(r.exited, pid)
		r.mu.Unlock()
		return st.st, nil
	}
	ch := make(chan ExitStatus, 1)
	r.waiters[pid] = ch
	r.mu.Unlock()
	r.kick()
	select {
	case st := <-ch:
		return st, nil
	case <-r.stop:
		return ExitStatus{}, errors.New("channel: reaper closed")
	}
}

// SignalGroup signals the process group of a leader we started, then the
// leader itself if the group signal fails.
func SignalGroup(pid int, sig syscall.Signal) error {
	if pid <= 1 {
		return errors.New("channel: refusing to signal pid <= 1")
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		return syscall.Kill(pid, sig)
	}
	return nil
}

// Forward delivers sig to every process group this reaper still tracks.
func (r *Reaper) Forward(sig syscall.Signal) {
	r.mu.Lock()
	pids := make([]int, 0, len(r.groups))
	for pid := range r.groups {
		pids = append(pids, pid)
	}
	r.mu.Unlock()
	for _, pid := range pids {
		_ = SignalGroup(pid, sig)
	}
}

func (r *Reaper) reap() {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		if pid <= 0 || err != nil {
			return
		}
		st := ExitStatus{Exited: ws.Exited(), Code: ws.ExitStatus()}
		if ws.Signaled() {
			st.Exited = false
			st.Signal = int(ws.Signal())
			st.Code = 128 + st.Signal
		}
		r.deliver(pid, st)
	}
}

func (r *Reaper) deliver(pid int, st ExitStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.groups, pid)
	if ch, ok := r.waiters[pid]; ok {
		delete(r.waiters, pid)
		ch <- st
		return
	}
	r.exited[pid] = savedExit{st: st, at: time.Now()}
	if len(r.exited) < 64 {
		return
	}
	cutoff := time.Now().Add(-time.Minute)
	for id, saved := range r.exited {
		if saved.at.Before(cutoff) {
			delete(r.exited, id)
		}
	}
}

func enableChildSubreaper() {
	// PR_SET_CHILD_SUBREAPER = 36. Orphans of commands we spawn are reparented
	// to this process so the wait loop can reap them.
	_, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, 36, 1, 0)
	_ = errno
}
