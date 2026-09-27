package bot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"strings"
	"time"

	"go.uber.org/zap"

	botsandbox "github.com/kasuganosora/thinkbot/docker/sandbox"
)

// browserMCPPath is where the builtin image installs the browser MCP wrapper.
const browserMCPPath = "/usr/local/bin/thinkbot-browser-mcp"

// syncBrowserMCPScript copies the browser MCP wrapper embedded in this binary
// into a running bot container built from the builtin image when the
// container's copy differs. Existing containers are never recreated when the
// builtin image tag changes (that would drop container-local installs), so
// without this new browser tools (select_option, check, form_fields) and
// error hints would never reach running bots. Best effort: failures are
// logged and the container's own script keeps working.
func syncBrowserMCPScript(ctx context.Context, container string, logger *zap.SugaredLogger) {
	want, err := botsandbox.BrowserMCPScript()
	if err != nil || len(want) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	img, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.Config.Image}}", container).Output()
	if err != nil {
		logger.Debugw("browser mcp sync: inspect failed", "container", container, "err", err)
		return
	}
	if !strings.HasPrefix(strings.TrimSpace(string(img)), botsandbox.BuiltinImagePrefix) {
		return // custom image: leave its wrapper alone
	}

	sum := sha256.Sum256(want)
	wantHash := hex.EncodeToString(sum[:])
	out, err := exec.CommandContext(ctx, "docker", "exec", container, "sha256sum", browserMCPPath).Output()
	if err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), wantHash) {
		return // up to date
	}

	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", container, "sh", "-c",
		"cat > "+browserMCPPath+".new && chmod 755 "+browserMCPPath+".new && mv -f "+browserMCPPath+".new "+browserMCPPath)
	cmd.Stdin = bytes.NewReader(want)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		logger.Warnw("browser mcp sync: write failed, keeping the container's script",
			"container", container, "err", err, "stderr", strings.TrimSpace(stderr.String()))
		return
	}
	logger.Infow("browser mcp script synced into container", "container", container, "sha256", wantHash[:12])
}
