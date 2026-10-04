package sandbox

import (
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestDesktopRFBShowsDisplayAndPointer(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	if _, err := exec.LookPath("Xvfb"); err != nil {
		t.Skip("Xvfb is not installed")
	}
	script, err := materializeDesktopScript(mustDesktopScript(t))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", script)
	cmd.Env = append(os.Environ(),
		"THINKBOT_DESKTOP_MODE=host",
		"THINKBOT_DESKTOP_DISPLAY="+hostDisplay("desktop-test"),
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()
	done := make(chan struct{})
	go func() {
		select {
		case <-time.After(8 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		case <-done:
		}
	}()
	defer close(done)

	if _, err := stdin.Write([]byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	ver := make([]byte, 12)
	if _, err := io.ReadFull(stdout, ver); err != nil {
		t.Fatal(err)
	}
	sec := make([]byte, 2)
	if _, err := io.ReadFull(stdout, sec); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(stdout, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	init := make([]byte, 24)
	if _, err := io.ReadFull(stdout, init); err != nil {
		t.Fatal(err)
	}
	w := binary.BigEndian.Uint16(init[0:2])
	h := binary.BigEndian.Uint16(init[2:4])
	if w == 0 || h == 0 {
		t.Fatalf("desktop size %dx%d", w, h)
	}
	nameLen := binary.BigEndian.Uint32(init[20:24])
	if _, err := io.ReadFull(stdout, make([]byte, nameLen)); err != nil {
		t.Fatal(err)
	}
	// pointer then a full-frame request
	ptr := make([]byte, 6)
	ptr[0] = 5
	binary.BigEndian.PutUint16(ptr[2:], 90)
	binary.BigEndian.PutUint16(ptr[4:], 70)
	if _, err := stdin.Write(ptr); err != nil {
		t.Fatal(err)
	}
	req := make([]byte, 10)
	req[0] = 3
	binary.BigEndian.PutUint16(req[6:], w)
	binary.BigEndian.PutUint16(req[8:], h)
	if _, err := stdin.Write(req); err != nil {
		t.Fatal(err)
	}
	hdr := make([]byte, 16)
	if _, err := io.ReadFull(stdout, hdr); err != nil {
		t.Fatal(err)
	}
	rw := int(binary.BigEndian.Uint16(hdr[8:10]))
	rh := int(binary.BigEndian.Uint16(hdr[10:12]))
	pix := make([]byte, rw*rh*4)
	if _, err := io.ReadFull(stdout, pix); err != nil {
		t.Fatal(err)
	}
	whites := 0
	for i := 0; i+3 < len(pix); i += 4 {
		if pix[i] > 240 && pix[i+1] > 240 && pix[i+2] > 240 {
			whites++
		}
	}
	if whites < 5 {
		t.Fatalf("pointer did not land on the X display (white pixels=%d)", whites)
	}
}

func mustDesktopScript(t *testing.T) []byte {
	t.Helper()
	b, err := desktopScriptBytes()
	if err != nil {
		t.Fatal(err)
	}
	return b
}
