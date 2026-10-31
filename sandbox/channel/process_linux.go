//go:build linux

package channel

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// SignalOthers delivers sig to every other process in this PID namespace.
// It is a no-op unless this process is PID 1, so a developer running the
// daemon on a host (Ctrl-C) cannot signal unrelated processes.
func SignalOthers(sig syscall.Signal) {
	if os.Getpid() != 1 {
		return
	}
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		_ = syscall.Kill(pid, sig)
	}
}

func listProcesses() ([]Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	page := int64(os.Getpagesize())
	out := make([]Process, 0, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		comm, ppid, rssPages, ok := parseProcStat(stat)
		if !ok {
			continue
		}
		cmd := comm
		if raw, err := os.ReadFile("/proc/" + e.Name() + "/cmdline"); err == nil {
			pretty := strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", " "))
			if pretty != "" {
				cmd = pretty
			}
		}
		out = append(out, Process{PID: pid, PPID: ppid, Cmd: cmd, RSS: rssPages * page})
	}
	return out, nil
}

// parseProcStat reads /proc/pid/stat. comm may contain spaces and parentheses;
// the last ')' closes it. rss is field 24 (index 21 of the post-comm fields).
func parseProcStat(b []byte) (comm string, ppid int, rss int64, ok bool) {
	open := indexByte(b, '(')
	close := lastByte(b, ')')
	if open < 0 || close < 0 || close < open {
		return "", 0, 0, false
	}
	comm = string(b[open+1 : close])
	rest := strings.Fields(string(b[close+1:]))
	// rest[0] is state. Need ppid (1) and rss (21).
	if len(rest) <= 21 {
		return "", 0, 0, false
	}
	ppid, err := strconv.Atoi(rest[1])
	if err != nil {
		return "", 0, 0, false
	}
	rss, err = strconv.ParseInt(rest[21], 10, 64)
	if err != nil {
		return "", 0, 0, false
	}
	return comm, ppid, rss, true
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func lastByte(b []byte, c byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func parseSignal(name string) (syscall.Signal, error) {
	s := strings.TrimSpace(name)
	if s == "" {
		return 0, errors.New("empty signal")
	}
	s = strings.TrimPrefix(strings.ToUpper(s), "SIG")
	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 || n >= 64 {
			return 0, errors.New("signal out of range")
		}
		return syscall.Signal(n), nil
	}
	switch s {
	case "HUP":
		return syscall.SIGHUP, nil
	case "INT":
		return syscall.SIGINT, nil
	case "QUIT":
		return syscall.SIGQUIT, nil
	case "ILL":
		return syscall.SIGILL, nil
	case "TRAP":
		return syscall.SIGTRAP, nil
	case "ABRT":
		return syscall.SIGABRT, nil
	case "BUS":
		return syscall.SIGBUS, nil
	case "FPE":
		return syscall.SIGFPE, nil
	case "KILL":
		return syscall.SIGKILL, nil
	case "USR1":
		return syscall.SIGUSR1, nil
	case "SEGV":
		return syscall.SIGSEGV, nil
	case "USR2":
		return syscall.SIGUSR2, nil
	case "PIPE":
		return syscall.SIGPIPE, nil
	case "ALRM":
		return syscall.SIGALRM, nil
	case "TERM":
		return syscall.SIGTERM, nil
	case "STOP":
		return syscall.SIGSTOP, nil
	case "CONT":
		return syscall.SIGCONT, nil
	default:
		return 0, errors.New("unknown signal " + name)
	}
}

func signalPID(pid int, sig syscall.Signal) error {
	if pid <= 1 || pid == os.Getpid() {
		return errors.New("refusing to signal the channel process")
	}
	if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
		return err
	}
	return syscall.Kill(pid, sig)
}
