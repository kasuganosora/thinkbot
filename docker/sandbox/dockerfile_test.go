package botsandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/sandbox/channel"
)

func TestDockerfileChannelIsPID1(t *testing.T) {
	data, err := buildContext.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `ENTRYPOINT ["/usr/local/bin/thinkbot-channel"]`) {
		t.Fatal("Dockerfile ENTRYPOINT must be the channel binary in exec form")
	}
	for _, line := range strings.Split(text, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "#") {
			continue
		}
		if strings.HasPrefix(trim, "ENTRYPOINT") && strings.Contains(trim, "sh") {
			t.Fatalf("shell ENTRYPOINT: %s", trim)
		}
		if strings.HasPrefix(trim, "CMD") && strings.Contains(trim, "sleep") {
			t.Fatalf("sleep CMD: %s", trim)
		}
	}
	if !strings.Contains(text, `CMD ["/usr/local/bin/thinkbot-channel", "health"]`) {
		t.Fatal("HEALTHCHECK must exec thinkbot-channel health")
	}
	if !strings.Contains(text, `thinkbot.channel.version="`+channel.Version+`"`) {
		t.Fatalf("LABEL version must match channel.Version %s", channel.Version)
	}
	if !strings.Contains(text, `thinkbot.channel.protocol="`+channel.Protocol+`"`) {
		t.Fatalf("LABEL protocol must match channel.Protocol %s", channel.Protocol)
	}
}

func TestWriteChannelBinary(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	dir := t.TempDir()
	if err := writeChannelBinary(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, channelBinaryName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 1<<20 {
		t.Fatalf("channel binary unexpectedly small: %d", info.Size())
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("channel binary is not executable")
	}
}

func TestChannelSourceDigestStable(t *testing.T) {
	a, err := channelSourceDigest()
	if err != nil {
		t.Fatal(err)
	}
	b, err := channelSourceDigest()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 16 || string(a) != string(b) {
		t.Fatalf("digest not stable: %x %x", a, b)
	}
}
