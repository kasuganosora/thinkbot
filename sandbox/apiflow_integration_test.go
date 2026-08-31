//go:build docker_integration

// 模拟文件管理 API 在 docker 隔离模式下的调用序列（上传/列目录/下载/销毁）。
package sandbox

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
)

// 模拟 API：botFileUpload → getBotFileEntries → serveBotFileDownload → handleRemoveBotContainer。
func TestBotContainer_ApiFlowSimulation(t *testing.T) {
	cfg := Config{
		Backend: "docker", Image: "alpine:latest",
		MemoryLimit: "256m", CPULimit: "0.5",
		Timeout: 15 * time.Second, MaxOutput: 64 * 1024, MaxFileWrite: 1 << 20,
		Timezone: "UTC",
	}
	mgr, err := NewBotWorkspaceManager(t.TempDir(), cfg, zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	defer mgr.Close()

	botID := "api-flow-bot"
	defer mgr.DestroyBot(botID, true)

	ws, err := mgr.GetOrCreate(botID)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	ctx := context.Background()

	// 1) 模拟 upload：写入子目录文件
	if err := ws.WriteFile(ctx, "docs/readme.txt", []byte("hello api flow")); err != nil {
		t.Fatalf("upload/WriteFile: %v", err)
	}

	// 2) 模拟 getBotFileEntries("/")：应列出 docs 目录，带 mtime
	entries, err := ws.ListDir(ctx, "/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	var foundDocs bool
	for _, e := range entries {
		if e.Name == "docs" && e.IsDir {
			foundDocs = true
			if e.ModTime.IsZero() {
				t.Logf("warn: docs mtime is zero (acceptable fallback)")
			}
		}
	}
	if !foundDocs {
		t.Fatalf("expected 'docs' dir in root listing, got %+v", entries)
	}

	// 3) 模拟 getBotFileEntries("docs")：列出 readme.txt，file 类型 + size
	sub, err := ws.ListDir(ctx, "docs")
	if err != nil {
		t.Fatalf("ListDir docs: %v", err)
	}
	if len(sub) != 1 || sub[0].Name != "readme.txt" || sub[0].IsDir || sub[0].Size == 0 {
		t.Fatalf("unexpected docs listing: %+v", sub)
	}

	// 4) 模拟 serveBotFileDownload：从容器读回内容
	data, err := ws.ReadFile(ctx, "docs/readme.txt")
	if err != nil {
		t.Fatalf("download/ReadFile: %v", err)
	}
	if string(data) != "hello api flow" {
		t.Fatalf("download content mismatch: %q", string(data))
	}

	t.Logf("API flow OK: upload→list(root+sub)→download all served from container")
}
