package loader

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseBool(t *testing.T) {
	cases := map[string]bool{
		"true": true, "1": true, "yes": true, "on": true, "enable": true, "enabled": true,
		"TRUE": true, "Yes": true,
		"false": false, "0": false, "no": false, "off": false, "disable": false, "": false, "foo": false,
	}
	for in, want := range cases {
		if got := parseBool(in); got != want {
			t.Errorf("parseBool(%q)=%v want %v", in, got, want)
		}
	}
}

func TestDeriveHealthURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "http://127.0.0.1:8080/health"},
		{":8080", "http://127.0.0.1:8080/health"},
		{"127.0.0.1:8080", "http://127.0.0.1:8080/health"},
		{"0.0.0.0:8080", "http://0.0.0.0:8080/health"},
		{"8080", "http://127.0.0.1:8080/health"},
	}
	for _, c := range cases {
		if got := deriveHealthURL(c.in); got != c.want {
			t.Errorf("deriveHealthURL(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	env := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(env, []byte("api.addr=:8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(env, "/app")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled {
		t.Error("default Enabled should be false")
	}
	if cfg.OpsAddr != "127.0.0.1:8090" {
		t.Errorf("OpsAddr=%q", cfg.OpsAddr)
	}
	if cfg.ChildBin != "/app/thinkbot" {
		t.Errorf("ChildBin=%q", cfg.ChildBin)
	}
	if cfg.PrevBin != "/app/thinkbot.prev" {
		t.Errorf("PrevBin=%q", cfg.PrevBin)
	}
	if cfg.NewBin != "/app/thinkbot.new" {
		t.Errorf("NewBin=%q", cfg.NewBin)
	}
	if cfg.HealthAddr != "http://127.0.0.1:8080/health" {
		t.Errorf("HealthAddr=%q", cfg.HealthAddr)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	env := filepath.Join(t.TempDir(), ".env")
	content := "loader.enabled=true\nloader.ops_addr=0.0.0.0:9090\nloader.token=secret\n" +
		"loader.smoke_port=:18080\nloader.crashloop_max=3\napi.addr=127.0.0.1:9099\n"
	if err := os.WriteFile(env, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(env, "/app")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled {
		t.Error("Enabled should be true")
	}
	if cfg.OpsAddr != "0.0.0.0:9090" {
		t.Errorf("OpsAddr=%q", cfg.OpsAddr)
	}
	if cfg.Token != "secret" {
		t.Errorf("Token=%q", cfg.Token)
	}
	if cfg.SmokePort != ":18080" {
		t.Errorf("SmokePort=%q", cfg.SmokePort)
	}
	if cfg.CrashLoopMax != 3 {
		t.Errorf("CrashLoopMax=%d", cfg.CrashLoopMax)
	}
	if cfg.HealthAddr != "http://127.0.0.1:9099/health" {
		t.Errorf("HealthAddr=%q", cfg.HealthAddr)
	}
}

func TestTailLines(t *testing.T) {
	data := []byte("a\nb\nc\nd\ne\n")
	if string(tailLines(data, 2)) != "d\ne\n" {
		t.Errorf("tailLines(2)=%q", tailLines(data, 2))
	}
	// 行数不超过时返回原样
	if string(tailLines(data, 10)) != string(data) {
		t.Error("tailLines(10) should return all")
	}
}

func TestWriteSmokeEnv(t *testing.T) {
	env := filepath.Join(t.TempDir(), ".env")
	base := "api.addr=:8080\nlog.level=info\nloader.enabled=true\n"
	if err := os.WriteFile(env, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(env, "/app")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := writeSmokeEnv(cfg, tmp); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(tmp, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !containsLine(s, "api.addr=:18080") {
		t.Errorf("smoke .env missing rewritten api.addr, got:\n%s", s)
	}
	if !containsLine(s, "log.level=info") {
		t.Errorf("smoke .env dropped other keys, got:\n%s", s)
	}
}

func containsLine(s, want string) bool {
	for _, line := range splitLines(s) {
		if line == want {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == '\n' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(c)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
