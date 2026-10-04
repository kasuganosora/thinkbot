package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/gorilla/websocket"

	botsandbox "github.com/kasuganosora/thinkbot/docker/sandbox"
	"github.com/kasuganosora/thinkbot/util/errs"
)

const desktopScriptPath = "/usr/local/bin/thinkbot-desktop"

// DesktopRunner starts the process that serves this bot's X display on stdio.
type DesktopRunner interface {
	StartDesktop(ctx context.Context) (*exec.Cmd, error)
}

// DesktopStatus is what the web workspace can open for one bot.
// Available means the websocket proxy is part of this build. A connection
// still fails if that bot's container is down or has no X display.
type DesktopStatus struct {
	Available bool   `json:"available"`
	Surface   string `json:"surface,omitempty"`
	Reason    string `json:"reason"`
}

func DescribeDesktop() DesktopStatus {
	return DesktopStatus{
		Available: true,
		Surface:   "xvfb-rfb",
		Reason:    "the bot's own X display is proxied on the session websocket; pointer and keyboard go to that display",
	}
}

var hostDisplays = struct {
	sync.Mutex
	next int
	by   map[string]string
}{next: 120, by: map[string]string{}}

func hostDisplay(botID string) string {
	hostDisplays.Lock()
	defer hostDisplays.Unlock()
	if d, ok := hostDisplays.by[botID]; ok {
		return d
	}
	hostDisplays.next++
	d := fmt.Sprintf(":%d", hostDisplays.next)
	hostDisplays.by[botID] = d
	return d
}

func (w *botWorkspace) StartDesktop(ctx context.Context) (*exec.Cmd, error) {
	script, err := botsandbox.DesktopScript()
	if err != nil {
		return nil, err
	}
	if w.container != nil {
		if err := w.container.ensure(ctx); err != nil {
			return nil, err
		}
		if err := SyncContainerBytes(ctx, w.container.container, desktopScriptPath, script); err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, "docker", "exec", "-i",
			"-e", "THINKBOT_DESKTOP_MODE=container",
			w.container.container, "node", desktopScriptPath)
		return cmd, nil
	}
	if w.backend != "local" {
		return nil, errs.New("sandbox: desktop needs the persistent bot container or the local backend")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, errs.New("sandbox: desktop needs node on the host for the local backend")
	}
	path, err := materializeDesktopScript(script)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, node, path)
	cmd.Env = append(os.Environ(),
		"THINKBOT_DESKTOP_MODE=host",
		"THINKBOT_DESKTOP_DISPLAY="+hostDisplay(w.botID),
	)
	return cmd, nil
}

func materializeDesktopScript(script []byte) (string, error) {
	sum := sha256.Sum256(script)
	path := filepath.Join(os.TempDir(), "thinkbot-desktop-"+hex.EncodeToString(sum[:8])+".js")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.WriteFile(path, script, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

func SyncContainerBytes(ctx context.Context, container, dest string, want []byte) error {
	sum := sha256.Sum256(want)
	wantHash := hex.EncodeToString(sum[:])
	out, err := exec.CommandContext(ctx, "docker", "exec", container, "sha256sum", dest).Output()
	if err == nil && len(out) >= len(wantHash) && string(out[:len(wantHash)]) == wantHash {
		return nil
	}
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", container, "sh", "-c",
		"cat > "+dest+".new && chmod 755 "+dest+".new && mv -f "+dest+".new "+dest)
	cmd.Stdin = bytes.NewReader(want)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return errs.Newf("sandbox: sync %s: %v %s", dest, err, stderr.String())
	}
	return nil
}

// RelayDesktop copies an RFB stdio process onto a websocket. The process is
// killed when the websocket closes or ctx is cancelled.
func RelayDesktop(ctx context.Context, conn *websocket.Conn, cmd *exec.Cmd) error {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()
	errc := make(chan error, 2)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := stdout.Read(buf)
			if n > 0 {
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					errc <- werr
					return
				}
			}
			if rerr != nil {
				if rerr != io.EOF {
					errc <- rerr
				} else {
					errc <- io.EOF
				}
				return
			}
		}
	}()
	go func() {
		for {
			_, data, rerr := conn.ReadMessage()
			if rerr != nil {
				errc <- rerr
				return
			}
			if _, werr := stdin.Write(data); werr != nil {
				errc <- werr
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errc:
		if err == io.EOF {
			return nil
		}
		return err
	}
}

func desktopScriptBytes() ([]byte, error) { return botsandbox.DesktopScript() }
