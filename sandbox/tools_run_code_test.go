package sandbox

import (
	"testing"
)

func TestRunCodeLang(t *testing.T) {
	cases := []struct {
		lang    string
		wantExt string
		wantCmd string
		wantErr bool
	}{
		{"bash", ".sh", "bash", false},
		{"sh", ".sh", "bash", false},
		{"python", ".py", "python3", false},
		{"py", ".py", "python3", false},
		{"node", ".js", "node", false},
		{"js", ".js", "node", false},
		{"ruby", "", "", true},
		{"", "", "", true},
	}
	for _, c := range cases {
		ext, cmd, err := runCodeLang(c.lang)
		if c.wantErr {
			if err == nil {
				t.Errorf("runCodeLang(%q): expected error", c.lang)
			}
			continue
		}
		if err != nil {
			t.Errorf("runCodeLang(%q): unexpected error %v", c.lang, err)
			continue
		}
		if ext != c.wantExt || cmd != c.wantCmd {
			t.Errorf("runCodeLang(%q) = (%q,%q), want (%q,%q)", c.lang, ext, cmd, c.wantExt, c.wantCmd)
		}
	}
}

func TestRunCodeToolDefinition(t *testing.T) {
	tool := buildRunCodeTool(nil, "test-bot")
	if tool.Name != "run_code" {
		t.Fatalf("tool name = %q, want run_code", tool.Name)
	}
	if tool.Execute == nil {
		t.Fatal("tool Execute must not be nil")
	}
	params, ok := tool.Parameters.(map[string]any)
	if !ok {
		t.Fatal("Parameters is not a map")
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("Parameters.properties missing")
	}
	for _, p := range []string{"code", "lang", "workdir", "timeout", "stuck_timeout"} {
		if _, ok := props[p]; !ok {
			t.Errorf("missing parameter %q", p)
		}
	}
	required, ok := params["required"].([]string)
	if !ok {
		t.Fatal("Parameters.required missing")
	}
	found := false
	for _, r := range required {
		if r == "code" {
			found = true
		}
	}
	if !found {
		t.Error("code must be required")
	}
}
