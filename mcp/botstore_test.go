package mcp

import (
	"os"
	"testing"
)

func TestBotStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	// BotStoreFile is relative to CWD; chdir into temp.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	botID := "bot-test-1"
	entries := []BotServerEntry{{
		ID:      "mcp-1",
		Name:    "filesystem",
		Type:    "stdio",
		Command: "npx",
		Args:    []string{"-y", "@modelcontextprotocol/server-filesystem", "."},
		Env:     map[string]string{"FOO": "bar"},
		Enabled: true,
		Status:  "draft",
	}}
	if err := SaveBotServers(botID, entries); err != nil {
		t.Fatalf("SaveBotServers: %v", err)
	}
	path := BotStoreFile(botID)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file at %s: %v", path, err)
	}
	loaded, err := LoadBotServers(botID)
	if err != nil {
		t.Fatalf("LoadBotServers: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Name != "filesystem" || loaded[0].Env["FOO"] != "bar" {
		t.Fatalf("unexpected loaded: %+v", loaded)
	}

	// missing bot → empty
	empty, err := LoadBotServers("no-such-bot")
	if err != nil || empty != nil {
		t.Fatalf("expected nil,nil for missing, got %v %v", empty, err)
	}
}

func TestBotServerEntry_ToServerConfig(t *testing.T) {
	e := BotServerEntry{
		Name:    "remote",
		Type:    "sse",
		URL:     "https://example.com/sse",
		Headers: map[string]string{"Authorization": "Bearer x"},
		Enabled: true,
	}
	cfg := e.ToServerConfig()
	if cfg.Transport != "http" {
		t.Fatalf("sse should map to http transport, got %q", cfg.Transport)
	}
	if !cfg.Enabled {
		t.Fatal("expected enabled")
	}
	if cfg.Headers["Authorization"] != "Bearer x" {
		t.Fatal("headers not copied")
	}

	draft := BotServerEntry{Name: "x", Type: "stdio", Enabled: true}
	if draft.ToServerConfig().Enabled {
		t.Fatal("draft without command must not be Enabled in runtime config")
	}
}

func TestValidateServerName(t *testing.T) {
	if err := ValidateServerName("browser"); err == nil {
		t.Fatal("browser must be reserved")
	}
	if err := ValidateServerName("bad name"); err == nil {
		t.Fatal("whitespace must be rejected")
	}
	if err := ValidateServerName("ok_server"); err != nil {
		t.Fatal(err)
	}
}

func TestDeriveStatus(t *testing.T) {
	if got := DeriveStatus(false, true, false, nil); got != "disabled" {
		t.Fatalf("got %s", got)
	}
	if got := DeriveStatus(true, false, false, nil); got != "draft" {
		t.Fatalf("got %s", got)
	}
	if got := DeriveStatus(true, true, true, nil); got != "running" {
		t.Fatalf("got %s", got)
	}
	if got := DeriveStatus(true, true, false, os.ErrNotExist); got != "error" {
		t.Fatalf("got %s", got)
	}
}
