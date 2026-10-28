package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kasuganosora/thinkbot/sandbox/channel"
)

func TestDaemonConfigIgnoresSleepInfinity(t *testing.T) {
	t.Setenv(channel.EnvSock, "")
	t.Setenv(channel.EnvRoot, "")
	t.Setenv(channel.EnvToken, "tok")
	t.Setenv(channel.EnvBotID, "bot-1")
	cfg, extra := daemonConfig([]string{"sleep", "infinity"})
	if len(extra) != 2 || extra[0] != "sleep" || extra[1] != "infinity" {
		t.Fatalf("extra %#v", extra)
	}
	if cfg.SocketPath != "" || cfg.Root != "" {
		t.Fatalf("flags should stay empty so Listen applies defaults, got %+v", cfg)
	}
	if cfg.Token != "" || cfg.BotID != "bot-1" {
		t.Fatalf("env not applied: %+v", cfg)
	}
	cfg, extra = daemonConfig([]string{"--sock", filepath.Join(os.TempDir(), "c.sock"), "--root", "/data", "sleep"})
	if cfg.SocketPath == "" || cfg.Root != "/data" {
		t.Fatalf("cfg %+v", cfg)
	}
	if len(extra) != 1 || extra[0] != "sleep" {
		t.Fatalf("extra %#v", extra)
	}
}

func TestConsumeChannelTokenFileOnce(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "channel.sock")
	tokenPath := filepath.Join(dir, channel.TokenFileName)
	if err := os.WriteFile(tokenPath, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(channel.EnvToken, "from-env")
	got := consumeChannelToken(sock)
	if got != "from-file" {
		t.Fatalf("got %q", got)
	}
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatal("token file was not removed")
	}
	if os.Getenv(channel.EnvToken) != "" {
		t.Fatal("token still in the environment")
	}
}

func TestConsumeChannelTokenUnsetsEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(channel.EnvToken, "from-env")
	got := consumeChannelToken(filepath.Join(dir, "channel.sock"))
	if got != "from-env" {
		t.Fatalf("got %q", got)
	}
	if os.Getenv(channel.EnvToken) != "" {
		t.Fatal("token still in the environment")
	}
}

func TestVersionLine(t *testing.T) {
	if channel.Version == "" || channel.Protocol == "" {
		t.Fatal("version constants empty")
	}
}
