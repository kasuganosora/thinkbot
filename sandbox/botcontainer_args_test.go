package sandbox

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/sandbox/channel"
	"github.com/kasuganosora/thinkbot/sandbox/channelclient"
)

func TestDialChannelRejectsCustomImage(t *testing.T) {
	c := newBotContainer("bot", Config{Image: "alpine:latest"}, zap.NewNop().Sugar())
	if _, err := c.DialChannel(context.Background()); err == nil {
		t.Fatal("custom image should not open a channel session")
	}
}

func TestDockerRunArgsChannelPID1(t *testing.T) {
	root := t.TempDir()
	c := newBotContainer("bot 1", Config{
		Timezone:        "UTC",
		MemoryLimit:     "2g",
		CPULimit:        "1.0",
		ChannelSecret:   "server-secret",
		ChannelSockRoot: root,
	}, zap.NewNop().Sugar())

	builtin := c.dockerRunArgs("thinkbot-bot:abc")
	if strings.Contains(strings.Join(builtin, "\x00"), "sleep") {
		t.Fatalf("builtin image must not be started with sleep: %v", builtin)
	}
	if builtin[len(builtin)-1] != "thinkbot-bot:abc" {
		t.Fatalf("last arg = %q", builtin[len(builtin)-1])
	}
	joined := strings.Join(builtin, " ")
	if !strings.Contains(joined, "thinkbot.channel.version="+channel.Version) || !strings.Contains(joined, "thinkbot.channel.protocol="+channel.Protocol) {
		t.Fatalf("missing channel labels: %s", joined)
	}
	hostDir := root + "/thinkbot-channel-bot-1"
	if !strings.Contains(joined, "-v "+hostDir+":"+channelclient.ContainerSockDir) {
		t.Fatalf("missing channel volume: %s", joined)
	}
	token, err := channelclient.DeriveToken("server-secret", "bot 1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joined, channel.EnvToken+"="+token) {
		t.Fatal("derived token was not injected")
	}
	if strings.Contains(joined, "server-secret") {
		t.Fatal("server secret leaked into docker args")
	}
	if !strings.Contains(joined, channel.EnvBotID+"=bot 1") {
		t.Fatalf("missing bot id env: %s", joined)
	}
	if os.Getuid() > 0 && !strings.Contains(joined, channel.EnvSockUID+"="+strconv.Itoa(os.Getuid())) {
		t.Fatalf("missing sock uid: %s", joined)
	}

	custom := c.dockerRunArgs("alpine:latest")
	if len(custom) < 3 || custom[len(custom)-3] != "alpine:latest" || custom[len(custom)-2] != "sleep" || custom[len(custom)-1] != "infinity" {
		t.Fatalf("custom image args = %v", custom)
	}
	if strings.Contains(strings.Join(custom, " "), "thinkbot.channel.version=") {
		t.Fatal("custom image should not be labeled as a channel PID 1")
	}
}
