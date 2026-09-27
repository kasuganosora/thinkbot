package sandbox

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

func TestOldStrNotFound_WhitespaceOnly(t *testing.T) {
	content := "func main() {\n\tfmt.Println(\"hi\")\n\treturn\n}\n"
	err := oldStrNotFoundError("main.go", content, "    fmt.Println(\"hi\")\n    return")
	msg := err.Error()
	if !strings.Contains(msg, "different whitespace") || !strings.Contains(msg, "lines 2-3") || !strings.Contains(msg, "\tfmt.Println") {
		t.Fatalf("unexpected: %s", msg)
	}
}

func TestOldStrNotFound_ClosestSnippet(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, "line number %d of filler text\n", i)
	}
	b.WriteString("const apiBase = \"https://api.odpt.org/api/v4\"\n")
	b.WriteString("const timeoutSeconds = 30\n")
	content := b.String()
	old := "const apiBase = \"https://api.odpt.org/api/v3\"\nconst timeoutSeconds = 20"
	msg := oldStrNotFoundError("cfg.go", content, old).Error()
	if !strings.Contains(msg, "Closest match (lines 501-502") || !strings.Contains(msg, "api/v4") {
		t.Fatalf("unexpected: %s", msg)
	}
	if !strings.Contains(msg, "Do not retry with the same old_str") {
		t.Error("should forbid blind retries")
	}
}

func TestOldStrNotFound_NothingSimilar(t *testing.T) {
	msg := oldStrNotFoundError("a.txt", "alpha\nbeta\n", "completely unrelated sentence here").Error()
	if !strings.Contains(msg, "Read the file again") {
		t.Fatalf("unexpected: %s", msg)
	}
}

func TestCheckWorkspacePath(t *testing.T) {
	for _, ok := range []string{"", "notes/a.md", "/data", "/data/tmp/x.md", "./x", "tmp/x"} {
		if err := checkWorkspacePath("path", ok); err != nil {
			t.Errorf("%q should be accepted: %v", ok, err)
		}
	}
	err := checkWorkspacePath("path", "/tmp/tt-notes/odpt-api.md")
	if err == nil || !strings.Contains(err.Error(), "/data/tmp/tt-notes/odpt-api.md") || !strings.Contains(err.Error(), "exec") {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestFileTools_RejectPathsOutsideWorkspace(t *testing.T) {
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	ctx := &llm.ToolExecContext{Context: context.Background()}
	w := buildWriteFileTool(mgr, botID)
	if _, err := w.Execute(ctx, map[string]any{"path": "/tmp/tt-notes/odpt-api.md", "content": "x"}); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("write_file /tmp must be refused: %v", err)
	}
	if _, err := w.Execute(ctx, map[string]any{"path": "/data/tmp/tt-notes/odpt-api.md", "content": "x"}); err != nil {
		t.Fatalf("write_file /data/tmp must work: %v", err)
	}
	r := buildReadFileTool(mgr, botID)
	if _, err := r.Execute(ctx, map[string]any{"path": "tmp/tt-notes/odpt-api.md"}); err != nil {
		t.Fatalf("relative read of the same file must work: %v", err)
	}
}

func TestReplaceInFile_ToolErrors(t *testing.T) {
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	ctx := &llm.ToolExecContext{Context: context.Background()}
	ws := getBotWS(t, mgr, botID)
	if err := ws.WriteFile(context.Background(), "main.go", []byte("package main\n\nfunc main() {\n\tprintln(1)\n}\n")); err != nil {
		t.Fatal(err)
	}
	tool := buildReplaceInFileTool(mgr, botID)
	_, err := tool.Execute(ctx, map[string]any{"path": "main.go", "old_str": "", "new_str": ""})
	if err == nil || !strings.Contains(err.Error(), "would change nothing") {
		t.Fatalf("empty old/new: %v", err)
	}
	_, err = tool.Execute(ctx, map[string]any{"path": "main.go", "old_str": "    println(1)", "new_str": "println(2)"})
	if err == nil || !strings.Contains(err.Error(), "different whitespace") || !strings.Contains(err.Error(), "\tprintln(1)") {
		t.Fatalf("mismatch hint: %v", err)
	}
}
