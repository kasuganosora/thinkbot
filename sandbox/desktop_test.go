package sandbox

import (
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"syscall"
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
		"THINKBOT_DESKTOP_MARKER=1",
		"THINKBOT_DESKTOP_CLIP_ACK=1",
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
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		_ = cmd.Wait()
	}()
	done := make(chan struct{})
	go func() {
		select {
		case <-time.After(15 * time.Second):
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
	w := int(binary.BigEndian.Uint16(init[0:2]))
	h := int(binary.BigEndian.Uint16(init[2:4]))
	if w != 1280 || h != 800 {
		t.Fatalf("want native 1280x800, got %dx%d", w, h)
	}
	nameLen := binary.BigEndian.Uint32(init[20:24])
	if _, err := io.ReadFull(stdout, make([]byte, nameLen)); err != nil {
		t.Fatal(err)
	}
	ptr := make([]byte, 6)
	ptr[0] = 5
	binary.BigEndian.PutUint16(ptr[2:], 90)
	binary.BigEndian.PutUint16(ptr[4:], 70)
	if _, err := stdin.Write(ptr); err != nil {
		t.Fatal(err)
	}
	req := make([]byte, 10)
	req[0] = 3
	binary.BigEndian.PutUint16(req[6:], uint16(w))
	binary.BigEndian.PutUint16(req[8:], uint16(h))
	if _, err := stdin.Write(req); err != nil {
		t.Fatal(err)
	}
	pix := make([]byte, w*h*4)
	gotPointer := false
	for tries := 0; tries < 8 && !gotPointer; tries++ {
		hdr := make([]byte, 4)
		if _, err := io.ReadFull(stdout, hdr); err != nil {
			t.Fatal(err)
		}
		if hdr[0] != 0 {
			t.Fatalf("framebuffer type %d", hdr[0])
		}
		n := int(binary.BigEndian.Uint16(hdr[2:4]))
		for i := 0; i < n; i++ {
			rh := make([]byte, 12)
			if _, err := io.ReadFull(stdout, rh); err != nil {
				t.Fatal(err)
			}
			x := int(binary.BigEndian.Uint16(rh[0:2]))
			y := int(binary.BigEndian.Uint16(rh[2:4]))
			rw := int(binary.BigEndian.Uint16(rh[4:6]))
			rhh := int(binary.BigEndian.Uint16(rh[6:8]))
			enc := binary.BigEndian.Uint32(rh[8:12])
			if enc != 0 {
				t.Fatalf("encoding %d", enc)
			}
			raw := make([]byte, rw*rhh*4)
			if _, err := io.ReadFull(stdout, raw); err != nil {
				t.Fatal(err)
			}
			for row := 0; row < rhh; row++ {
				copy(pix[((y+row)*w+x)*4:], raw[row*rw*4:(row+1)*rw*4])
			}
		}
		off := (70*w + 90) * 4
		if pix[off] > 240 && pix[off+1] > 240 && pix[off+2] > 240 {
			gotPointer = true
		} else if n > 0 {
			if _, err := stdin.Write(req); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !gotPointer {
		t.Fatal("pointer did not land on the native pixel")
	}
	// The old 2x scale would have painted the marker at (45,35). That pixel stays dark.
	scaled := (35*w + 45) * 4
	if pix[scaled] > 240 && pix[scaled+1] > 240 && pix[scaled+2] > 240 {
		t.Fatal("marker landed on the downscaled coordinate")
	}

	text := []byte("你好-clip")
	cut := make([]byte, 8+len(text))
	cut[0] = 6
	binary.BigEndian.PutUint32(cut[4:], uint32(len(text)))
	copy(cut[8:], text)
	if _, err := stdin.Write(cut); err != nil {
		t.Fatal(err)
	}
	ack := make([]byte, 8)
	if _, err := io.ReadFull(stdout, ack); err != nil {
		t.Fatal(err)
	}
	if ack[0] != 3 {
		t.Fatalf("clipboard ack type %d", ack[0])
	}
	n := binary.BigEndian.Uint32(ack[4:8])
	if n > 1024 {
		t.Fatalf("clipboard ack length %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(stdout, body); err != nil {
		t.Fatal(err)
	}
	if string(body) != string(text) {
		t.Fatalf("clipboard round trip %q", body)
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
