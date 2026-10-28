//go:build linux

package channel

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseProcStatCommWithSpace(t *testing.T) {
	// 22 post-comm fields; rss is the last one (field 24 of the file).
	rest := []string{"S", "7"}
	for len(rest) < 21 {
		rest = append(rest, "0")
	}
	rest = append(rest, "99")
	sample := "42 (my proc) " + strings.Join(rest, " ")
	comm, ppid, rss, ok := parseProcStat([]byte(sample))
	if !ok {
		t.Fatal("parse failed")
	}
	if comm != "my proc" || ppid != 7 || rss != 99 {
		t.Fatalf("comm=%q ppid=%d rss=%d", comm, ppid, rss)
	}
}

func TestListProcessesSeesSelf(t *testing.T) {
	procs, err := listProcesses()
	if err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	for _, p := range procs {
		if p.PID == self {
			if p.PPID != os.Getppid() {
				t.Fatalf("ppid %d, want %d", p.PPID, os.Getppid())
			}
			return
		}
	}
	t.Fatal("self pid missing from list")
}

func TestSignalOthersDoesNotKillWhenNotPID1(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("running as pid 1")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	SignalOthers(syscall.SIGKILL)
	time.Sleep(150 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("sleep died after SignalOthers while not PID 1: %v", err)
	}
}
