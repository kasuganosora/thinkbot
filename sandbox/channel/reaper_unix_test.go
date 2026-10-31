//go:build linux

package channel

import (
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReaperExitCode(t *testing.T) {
	r := NewReaper()
	defer r.Close()
	cmd := exec.Command("sh", "-c", "exit 3")
	if err := r.Start(cmd); err != nil {
		t.Fatal(err)
	}
	st, err := r.Wait(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if st.Code != 3 || !st.Exited {
		t.Fatalf("status %+v", st)
	}
	_ = cmd.Process.Release()
}

func TestReaperForwardStopsGroup(t *testing.T) {
	r := NewReaper()
	defer r.Close()
	cmd := exec.Command("sleep", "30")
	if err := r.Start(cmd); err != nil {
		t.Fatal(err)
	}
	r.Forward(syscall.SIGTERM)
	st, err := r.Wait(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if st.Signal != int(syscall.SIGTERM) && st.Code != 128+int(syscall.SIGTERM) {
		t.Fatalf("status %+v, want SIGTERM", st)
	}
	_ = cmd.Process.Release()
}

func TestReaperReapsDoubleFork(t *testing.T) {
	r := NewReaper()
	defer r.Close()
	cmd := exec.Command("sh", "-c", "sleep 30 >/dev/null 2>&1 & echo $!")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(cmd); err != nil {
		t.Fatal(err)
	}
	buf, err := io.ReadAll(stdout)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Wait(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Process.Release()
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf)))
	if err != nil || pid <= 1 {
		t.Fatalf("child pid %q: %v", buf, err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, statErr := os.Stat("/proc/" + strconv.Itoa(pid))
		if os.IsNotExist(statErr) {
			return
		}
		raw, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if strings.Contains(string(raw), ") Z ") || strings.Contains(string(raw), ") Z\t") {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		time.Sleep(20 * time.Millisecond)
	}
	raw, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	t.Fatalf("orphan pid %d was not reaped: %s", pid, raw)
}
