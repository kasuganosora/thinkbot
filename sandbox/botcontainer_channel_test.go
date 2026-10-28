package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/sandbox/channel"
	"github.com/kasuganosora/thinkbot/sandbox/channelclient"
)

func startChannelFixture(t *testing.T, botID string) (*channel.Server, *channelclient.Client) {
	t.Helper()
	token, err := channelclient.DeriveToken("server-secret", botID)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "tbw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	srv, err := channel.Listen(channel.Config{
		Root:        dir + "/data",
		Token:       token,
		BotID:       botID,
		SocketPath:  dir + "/s",
		MaxPutBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() { _ = srv.Close() })
	cli, err := channelclient.Dial(context.Background(), srv.Addr(), token, botID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return srv, cli
}

func TestChannelFileAdapterRoundTrip(t *testing.T) {
	_, cli := startChannelFixture(t, "bot-a")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := writeChannelFile(ctx, cli, "nested/a.txt", strings.NewReader("hello"), 0); err != nil {
		t.Fatal(err)
	}
	got, err := readChannelFile(ctx, cli, "nested/a.txt")
	if err != nil || string(got) != "hello" {
		t.Fatalf("read %q %v", got, err)
	}
	entries, err := listChannelDir(ctx, cli, "nested")
	if err != nil || len(entries) != 1 || entries[0].Name != "a.txt" || entries[0].Size != 5 {
		t.Fatalf("list %+v %v", entries, err)
	}
	if _, err := readChannelFile(ctx, cli, "missing"); err == nil || !os.IsNotExist(err) {
		t.Fatalf("missing err %v", err)
	}
}

func TestChannelExecAndStuckWatchdog(t *testing.T) {
	_, cli := startChannelFixture(t, "bot-a")
	c := newBotContainer("bot-a", Config{
		ChannelSecret: "server-secret",
		StuckTimeout:  time.Minute,
		Timeout:       time.Minute,
		MaxOutput:     1 << 20,
	}, zap.NewNop().Sugar())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := c.runChannelCommand(ctx, cli, ExecRequest{Command: "printf hi"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.Stdout != "hi" {
		t.Fatalf("exec %+v", res)
	}

	stuck := newBotContainer("bot-a", Config{
		ChannelSecret: "server-secret",
		MaxOutput:     1024,
	}, zap.NewNop().Sugar())
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	res, err = stuck.runChannelCommand(sctx, cli, ExecRequest{
		Command:      "sleep 30",
		StuckTimeout: 200 * time.Millisecond,
		Timeout:      10 * time.Second,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 || !res.Aborted {
		t.Fatalf("stuck result %+v", res)
	}
}
