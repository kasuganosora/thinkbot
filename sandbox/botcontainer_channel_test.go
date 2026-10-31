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

// TestChannelRecreateBackoffLevels 验证退避档位指数增长并封顶，
// 且失败次数极大时不会因重复倍增而溢出。
func TestChannelRecreateBackoffLevels(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{1, 30 * time.Second},
		{2, 60 * time.Second},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{5, 8 * time.Minute},
		{6, channelRecreateMaxBackoff},
		{50, channelRecreateMaxBackoff},
	}
	for _, tc := range cases {
		if got := channelRecreateBackoff(tc.failures); got != tc.want {
			t.Fatalf("channelRecreateBackoff(%d) = %s, want %s", tc.failures, got, tc.want)
		}
	}
}

// TestEnsureChannelLockedCooldownSuppressesRecreate 验证冷却窗口内探针失败时
// 不执行破坏性的「删容器 + 重建」。缺少这道闸时，重建修不好的环境级故障
// 会随调用方频率无限销毁重建容器。
func TestEnsureChannelLockedCooldownSuppressesRecreate(t *testing.T) {
	c := newBotContainer("bot-cooldown", Config{
		ChannelSecret: "server-secret",
		Image:         "thinkbot-bot:test",
	}, zap.NewNop().Sugar())
	until := time.Now().Add(time.Hour)
	c.channelRecreateUntil = until

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := c.ensureChannelLocked(ctx)
	if err == nil {
		t.Fatal("expected channel probe failure, got nil")
	}
	if !strings.Contains(err.Error(), "cooling down") {
		t.Fatalf("expected recreate suppressed by cooldown, got %v", err)
	}
	if !strings.Contains(err.Error(), "probe:") {
		t.Fatalf("probe should still run during cooldown, got %v", err)
	}
	// 冷却期内被跳过的重建不计入失败，也不刷新截止时间。
	if c.channelRecreateFails != 0 {
		t.Fatalf("suppressed recreate must not count, consecutive failures = %d", c.channelRecreateFails)
	}
	if !c.channelRecreateUntil.Equal(until) {
		t.Fatalf("cooldown window moved: %s -> %s", until, c.channelRecreateUntil)
	}

	c.clearChannelRecreateBackoff()
	if c.channelRecreateFails != 0 || !c.channelRecreateUntil.IsZero() || c.channelRecreateErr != "" {
		t.Fatalf("cooldown not cleared: fails=%d until=%v err=%q",
			c.channelRecreateFails, c.channelRecreateUntil, c.channelRecreateErr)
	}
}

// TestEnsureChannelLockedCooldownExpiresUnderContinuousEnsure 验证连续 ensure
// 在冷却窗口内不会把截止时间往后推。窗口到期后，破坏性重建可以再次执行；
// 那次重建若仍失败，才重新武装冷却。
func TestEnsureChannelLockedCooldownExpiresUnderContinuousEnsure(t *testing.T) {
	// 到期后的重建会调用 docker。清空 PATH 让它立刻失败，避免构建或拉取镜像。
	t.Setenv("PATH", t.TempDir())

	c := newBotContainer("bot-cooldown-expire", Config{
		ChannelSecret:   "server-secret",
		Image:           "thinkbot-bot:test",
		ChannelSockRoot: t.TempDir(),
	}, zap.NewNop().Sugar())

	window := 250 * time.Millisecond
	until := time.Now().Add(window)
	c.channelRecreateUntil = until
	c.channelRecreateFails = 2
	c.channelRecreateErr = "probe down"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	suppressed := 0
	for time.Now().Before(until.Add(-40 * time.Millisecond)) {
		err := c.ensureChannelLocked(ctx)
		if err == nil || !strings.Contains(err.Error(), "cooling down") {
			t.Fatalf("expected suppressed recreate, got %v", err)
		}
		if c.channelRecreateFails != 2 {
			t.Fatalf("suppressed ensure counted as failure: fails=%d until=%s", c.channelRecreateFails, c.channelRecreateUntil)
		}
		if !c.channelRecreateUntil.Equal(until) {
			t.Fatalf("cooldown extended from %s to %s", until, c.channelRecreateUntil)
		}
		suppressed++
		time.Sleep(30 * time.Millisecond)
	}
	if suppressed < 2 {
		t.Fatalf("expected multiple suppressed ensures, got %d", suppressed)
	}

	time.Sleep(time.Until(until) + 50*time.Millisecond)

	err := c.ensureChannelLocked(ctx)
	if err != nil && strings.Contains(err.Error(), "cooling down") {
		t.Fatalf("window elapsed but recreate still suppressed: until=%s err=%v", c.channelRecreateUntil, err)
	}
	if c.channelRecreateFails != 3 {
		t.Fatalf("destructive recreate failure not recorded, fails=%d err=%v", c.channelRecreateFails, err)
	}
	if !c.channelRecreateUntil.After(time.Now()) {
		t.Fatalf("expected cooldown re-armed after failed recreate, until=%s", c.channelRecreateUntil)
	}
}

// TestEnsureChannelLockedSuccessClearsCooldown 验证探针恢复成功时清掉冷却，
// 即使当前仍在窗口内（容器可能自己恢复，探针不能被冷却跳过）。
func TestEnsureChannelLockedSuccessClearsCooldown(t *testing.T) {
	const botID = "bot-clear"
	token, err := channelclient.DeriveToken("server-secret", botID)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "tbw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	sock := root + "/thinkbot-channel-" + botID + "/channel.sock"
	srv, err := channel.Listen(channel.Config{
		Root:        root + "/data",
		Token:       token,
		BotID:       botID,
		SocketPath:  sock,
		MaxPutBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	sctx, scancel := context.WithCancel(context.Background())
	t.Cleanup(scancel)
	go func() { _ = srv.Serve(sctx) }()
	t.Cleanup(func() { _ = srv.Close() })

	c := newBotContainer(botID, Config{
		ChannelSecret:   "server-secret",
		Image:           "thinkbot-bot:test",
		ChannelSockRoot: root,
	}, zap.NewNop().Sugar())
	c.channelRecreateUntil = time.Now().Add(time.Hour)
	c.channelRecreateFails = 4
	c.channelRecreateErr = "old failure"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ensureChannelLocked(ctx); err != nil {
		t.Fatalf("healthy probe should clear cooldown, got %v", err)
	}
	if c.channelRecreateFails != 0 || !c.channelRecreateUntil.IsZero() || c.channelRecreateErr != "" {
		t.Fatalf("cooldown not cleared on success: fails=%d until=%v err=%q",
			c.channelRecreateFails, c.channelRecreateUntil, c.channelRecreateErr)
	}
}
