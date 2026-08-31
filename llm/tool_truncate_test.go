package llm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTruncateOutputOffload 验证：当输出被截断且提供 OffloadSink 时，
// 完整原文落盘、返回预览+指针、且指向 spawn 委托提示。
func TestTruncateOutputOffload(t *testing.T) {
	dir := t.TempDir()
	var gotBotID, gotToolCallID string
	var gotContent []byte
	sink := func(botID, toolCallID string, content []byte) (string, error) {
		gotBotID = botID
		gotToolCallID = toolCallID
		gotContent = content
		p := filepath.Join(dir, botID, toolCallID+".txt")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			return "", err
		}
		return filepath.Join(botID, toolCallID+".txt"), nil
	}

	// 构造超过 50KB 阈值的输入（单行超长，触发字节级兜底或行截断均可）。
	big := strings.Repeat("line of code with keyword\n", 4000) // ~96KB

	res := TruncateOutput(big, DefaultTruncationConfig(), WithOffload("bot-1", "tc-9", sink))
	if !res.Truncated {
		t.Fatalf("expected truncated=true for large input")
	}
	if res.OffloadPath != "bot-1/tc-9.txt" {
		t.Fatalf("OffloadPath = %q, want bot-1/tc-9.txt", res.OffloadPath)
	}
	if gotBotID != "bot-1" || gotToolCallID != "tc-9" {
		t.Fatalf("sink called with botID=%q toolCallID=%q", gotBotID, gotToolCallID)
	}
	if string(gotContent) != big {
		t.Fatalf("offloaded content mismatch: got %d bytes want %d", len(gotContent), len(big))
	}
	preview, ok := res.Output.(string)
	if !ok {
		t.Fatalf("preview output is not string: %T", res.Output)
	}
	if !strings.Contains(preview, "bot-1/tc-9.txt") {
		t.Fatalf("preview missing pointer path:\n%s", preview)
	}
	if !strings.Contains(preview, "spawn") {
		t.Fatalf("preview missing spawn delegation hint:\n%s", preview)
	}
}

// TestTruncateOutputOffloadFailSafe 验证：sink 报错时退化成 head+tail 截断，
// 不抛出、OffloadPath 为空。
func TestTruncateOutputOffloadFailSafe(t *testing.T) {
	sink := func(botID, toolCallID string, content []byte) (string, error) {
		return "", os.ErrPermission
	}
	big := strings.Repeat("x", 60*1024)
	res := TruncateOutput(big, DefaultTruncationConfig(), WithOffload("b", "t", sink))
	if !res.Truncated {
		t.Fatalf("expected truncated")
	}
	if res.OffloadPath != "" {
		t.Fatalf("OffloadPath should be empty on sink failure, got %q", res.OffloadPath)
	}
	if _, ok := res.Output.(string); !ok {
		t.Fatalf("output should remain string on fail-safe")
	}
}

// TestTruncateOutputNoOffloadWithinLimit 验证：未超限时原样返回、不调用 sink、
// OffloadPath 为空。
func TestTruncateOutputNoOffloadWithinLimit(t *testing.T) {
	called := false
	sink := func(botID, toolCallID string, content []byte) (string, error) {
		called = true
		return "", nil
	}
	small := "short output"
	res := TruncateOutput(small, DefaultTruncationConfig(), WithOffload("b", "t", sink))
	if res.Truncated {
		t.Fatalf("small input should not be truncated")
	}
	if called {
		t.Fatalf("sink must not be called when not truncated")
	}
	if res.OffloadPath != "" {
		t.Fatalf("OffloadPath should be empty when not truncated")
	}
}
