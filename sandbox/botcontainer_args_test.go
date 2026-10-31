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

	builtin, err := c.dockerRunArgs("thinkbot-bot:abc")
	if err != nil {
		t.Fatal(err)
	}
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
	if !strings.Contains(joined, "thinkbot.channel.transport="+channel.TransportUnix) {
		t.Fatalf("linux default must stay unix: %s", joined)
	}
	if strings.Contains(joined, "-p ") || strings.Contains(joined, channel.EnvHTTP+"=") {
		t.Fatalf("unix transport must not publish tcp: %s", joined)
	}
	hostDir := root + "/thinkbot-channel-bot-1"
	if !strings.Contains(joined, "-v "+hostDir+":"+channelclient.ContainerSockDir) {
		t.Fatalf("missing channel volume: %s", joined)
	}
	token, err := channelclient.DeriveToken("server-secret", "bot 1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(joined, channel.EnvToken+"=") || strings.Contains(joined, token) || strings.Contains(joined, "server-secret") {
		t.Fatal("token or server secret leaked into docker args")
	}
	if err := c.writeChannelToken(token); err != nil {
		t.Fatal(err)
	}
	tokenPath := hostDir + "/" + channel.TokenFileName
	fi, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token mode %o", fi.Mode().Perm())
	}
	body, err := os.ReadFile(tokenPath)
	if err != nil || string(body) != token {
		t.Fatalf("token file %q", body)
	}
	if !strings.Contains(joined, channel.EnvBotID+"=bot 1") {
		t.Fatalf("missing bot id env: %s", joined)
	}
	if os.Getuid() > 0 && !strings.Contains(joined, channel.EnvSockUID+"="+strconv.Itoa(os.Getuid())) {
		t.Fatalf("missing sock uid: %s", joined)
	}

	custom, err := c.dockerRunArgs("alpine:latest")
	if err != nil {
		t.Fatal(err)
	}
	if len(custom) < 3 || custom[len(custom)-3] != "alpine:latest" || custom[len(custom)-2] != "sleep" || custom[len(custom)-1] != "infinity" {
		t.Fatalf("custom image args = %v", custom)
	}
	if strings.Contains(strings.Join(custom, " "), "thinkbot.channel.version=") {
		t.Fatal("custom image should not be labeled as a channel PID 1")
	}
}

func TestDockerRunArgsTCPPublishesLoopbackOnly(t *testing.T) {
	c := newBotContainer("bot 1", Config{
		ChannelTransport: "http",
		ChannelSecret:    "server-secret",
		ChannelSockRoot:  t.TempDir(),
	}, zap.NewNop().Sugar())
	args, err := c.dockerRunArgs("thinkbot-bot:abc")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-p "+channel.PublishSpec()) {
		t.Fatalf("missing loopback publish: %s", joined)
	}
	if strings.Contains(joined, "-p 0.0.0.0") || strings.Contains(joined, "-p "+strconv.Itoa(channel.PublishPort)+":") {
		t.Fatalf("publish is not host-loopback-only: %s", joined)
	}
	if !strings.Contains(joined, channel.EnvHTTP+"="+channel.ContainerListenAddr()) {
		t.Fatalf("missing listen addr env: %s", joined)
	}
	token, err := channelclient.DeriveToken("server-secret", "bot 1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(joined, channel.EnvToken+"=") || strings.Contains(joined, token) || strings.Contains(joined, "server-secret") {
		t.Fatal("token or server secret leaked into docker args")
	}
	if !strings.Contains(joined, "thinkbot.channel.transport="+channel.TransportTCP) {
		t.Fatalf("transport label: %s", joined)
	}
	if !strings.Contains(joined, "-v ") || !strings.Contains(joined, channelclient.ContainerSockDir) {
		t.Fatalf("token volume must stay mounted: %s", joined)
	}
}

func TestTCPTransportRejectsNetworkNone(t *testing.T) {
	c := newBotContainer("bot", Config{
		Image:            "builtin",
		ChannelTransport: "tcp",
		NetworkDisabled:  true,
	}, zap.NewNop().Sugar())
	if _, err := c.channelTransport(); err == nil {
		t.Fatal("tcp transport with network none should fail")
	}
	if _, err := c.dockerRunArgs("thinkbot-bot:abc"); err == nil {
		t.Fatal("docker args should refuse tcp with network none")
	}
}

func TestParsePublishedLoopback(t *testing.T) {
	raw := `{"17931/tcp":[{"HostIp":"127.0.0.1","HostPort":"55123"}]}`
	addr, err := parsePublishedLoopback(raw, channel.PublishPort)
	if err != nil || addr != "127.0.0.1:55123" {
		t.Fatalf("addr %q %v", addr, err)
	}
	v6 := `{"17931/tcp":[{"HostIp":"::1","HostPort":"9"}]}`
	addr, err = parsePublishedLoopback(v6, channel.PublishPort)
	if err != nil || addr != "[::1]:9" {
		t.Fatalf("v6 %q %v", addr, err)
	}
	for _, raw := range []string{
		`{"17931/tcp":[{"HostIp":"0.0.0.0","HostPort":"55123"}]}`,
		`{"17931/tcp":[{"HostIp":"","HostPort":"55123"}]}`,
		`{"17931/tcp":[{"HostIp":"10.0.0.8","HostPort":"55123"}]}`,
		`{}`,
		`null`,
	} {
		if _, err := parsePublishedLoopback(raw, channel.PublishPort); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
