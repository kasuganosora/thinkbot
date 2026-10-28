package sandbox

import (
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/sandbox/channel"
)

func TestDockerRunArgsChannelPID1(t *testing.T) {
	c := newBotContainer("bot 1", Config{
		Timezone:    "UTC",
		MemoryLimit: "2g",
		CPULimit:    "1.0",
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

	custom := c.dockerRunArgs("alpine:latest")
	if len(custom) < 3 || custom[len(custom)-3] != "alpine:latest" || custom[len(custom)-2] != "sleep" || custom[len(custom)-1] != "infinity" {
		t.Fatalf("custom image args = %v", custom)
	}
	if strings.Contains(strings.Join(custom, " "), "thinkbot.channel.version=") {
		t.Fatal("custom image should not be labeled as a channel PID 1")
	}
}
