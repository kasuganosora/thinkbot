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
	if cfg.Token != "tok" || cfg.BotID != "bot-1" {
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

func TestVersionLine(t *testing.T) {
	if channel.Version == "" || channel.Protocol == "" {
		t.Fatal("version constants empty")
	}
}
