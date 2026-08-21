package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// TestMemoryTool_CrossChannelMirror 验证写入 channel 作用域的记忆会被镜像到
// bot 全局作用域，且增/改/删都同步到镜像，使其他频道能召回跨平台活动。
func TestMemoryTool_CrossChannelMirror(t *testing.T) {
	repo := NewMemoryRepository()
	cfg := ToolConfig{Repo: repo, BotID: "bot-test"}
	ctx := &llm.ToolExecContext{Context: context.Background()}
	botScope := BotScope("bot-test")
	chScope := ChannelScope("web:1")

	// 1) add：channel 记忆应镜像到 bot 全局作用域（正文带 [web:1] 来源前缀）。
	if _, err := handleAdd(ctx, repo, cfg, chScope, map[string]any{
		"content": "你review下thinkbot的代码",
	}); err != nil {
		t.Fatalf("add failed: %v", err)
	}

	mirrors, err := repo.Retrieve(ctx, Query{Scopes: []Scope{botScope}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(mirrors) != 1 {
		t.Fatalf("expected 1 mirrored entry, got %d", len(mirrors))
	}
	if got := mirrors[0].Content; got != "[web:1] 你review下thinkbot的代码" {
		t.Errorf("mirror content = %q, want prefixed original", got)
	}
	if !strings.HasPrefix(mirrors[0].ID, crossChannelMirrorPrefix) {
		t.Errorf("mirror ID %q should be prefixed with %q", mirrors[0].ID, crossChannelMirrorPrefix)
	}
	// 原始 channel 记忆不被移除。
	if n, _ := repo.Count(ctx, chScope); n != 1 {
		t.Errorf("original channel entry should remain, count=%d", n)
	}

	// 2) replace：镜像应就地更新（仍为 1 条，内容同步）。
	if _, err := handleReplace(ctx, repo, cfg, chScope, map[string]any{
		"old_text": "review下thinkbot",
		"content":  "你commit没按英文格式书写",
	}); err != nil {
		t.Fatalf("replace failed: %v", err)
	}
	mirrors, _ = repo.Retrieve(ctx, Query{Scopes: []Scope{botScope}, Limit: 10})
	if len(mirrors) != 1 {
		t.Fatalf("replace should keep 1 mirror, got %d", len(mirrors))
	}
	if got := mirrors[0].Content; got != "[web:1] 你commit没按英文格式书写" {
		t.Errorf("updated mirror content = %q", got)
	}

	// 3) remove：镜像应被同步删除。
	if _, err := handleRemove(ctx, repo, cfg, chScope, map[string]any{
		"old_text": "commit没按英文",
	}); err != nil {
		t.Fatalf("remove failed: %v", err)
	}
	mirrors, _ = repo.Retrieve(ctx, Query{Scopes: []Scope{botScope}, Limit: 10})
	if len(mirrors) != 0 {
		t.Fatalf("remove should delete mirror, got %d", len(mirrors))
	}
}

// TestMemoryTool_CrossChannelMirrorDisabled 验证未配置 BotID 时不镜像。
func TestMemoryTool_CrossChannelMirrorDisabled(t *testing.T) {
	repo := NewMemoryRepository()
	cfg := ToolConfig{Repo: repo} // 无 BotID
	ctx := &llm.ToolExecContext{Context: context.Background()}

	if _, err := handleAdd(ctx, repo, cfg, ChannelScope("web:1"), map[string]any{
		"content": "孤立的频道记忆",
	}); err != nil {
		t.Fatalf("add failed: %v", err)
	}
	mirrors, _ := repo.Retrieve(ctx, Query{Scopes: []Scope{BotScope("")}, Limit: 10})
	if len(mirrors) != 0 {
		t.Fatalf("without BotID no mirror should be created, got %d", len(mirrors))
	}
}
