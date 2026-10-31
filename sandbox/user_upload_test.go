package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/llm"
)

// 工具写入继续受 MaxFileWrite（默认 10MB）约束；用户上传走流式 WriteUserUploadStream，不受该上限。
func TestDefaultMaxFileWriteStays10MiB(t *testing.T) {
	if got := DefaultConfig().MaxFileWrite; got != 10<<20 {
		t.Fatalf("MaxFileWrite = %d, want %d (tool writes stay at 10MiB)", got, 10<<20)
	}
}

func TestCopyUserUpload_ExactLimitOK_OverLimitErrors(t *testing.T) {
	var dst bytes.Buffer
	n, err := CopyUserUpload(&dst, strings.NewReader("01234567"), 8)
	if err != nil {
		t.Fatalf("exact limit: %v", err)
	}
	if n != 8 || dst.String() != "01234567" {
		t.Fatalf("copied %d %q", n, dst.String())
	}

	dst.Reset()
	n, err = CopyUserUpload(&dst, strings.NewReader("012345678"), 8)
	if !errors.Is(err, ErrUserUploadTooLarge) {
		t.Fatalf("over limit: n=%d err=%v", n, err)
	}
}

func TestBotWorkspace_ToolWriteRejectsOverMaxFileWrite_UserStreamAllowsIt(t *testing.T) {
	const maxTool = 64
	mgr, err := NewBotWorkspaceManager(t.TempDir(), Config{
		Backend:      "local",
		MaxFileWrite: maxTool,
	}, zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("NewBotWorkspaceManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	ws, err := mgr.GetOrCreate("bot-upload")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	bw, ok := ws.(*botWorkspace)
	if !ok {
		t.Fatalf("workspace type %T", ws)
	}

	over := bytes.Repeat([]byte{'x'}, maxTool+1)
	ctx := context.Background()
	if err := bw.WriteFile(ctx, "tool.bin", over); err == nil {
		t.Fatal("tool WriteFile accepted a payload over MaxFileWrite")
	} else if !strings.Contains(err.Error(), "exceeds max write") {
		t.Fatalf("WriteFile error = %v, want max-write rejection", err)
	}

	under := bytes.Repeat([]byte{'y'}, maxTool)
	if err := bw.WriteFile(ctx, "tool-ok.bin", under); err != nil {
		t.Fatalf("tool WriteFile at MaxFileWrite: %v", err)
	}

	n, err := bw.WriteUserUploadStream(ctx, "user.bin", bytes.NewReader(over), int64(len(over)))
	if err != nil {
		t.Fatalf("WriteUserUploadStream over MaxFileWrite: %v", err)
	}
	if n != int64(len(over)) {
		t.Fatalf("streamed %d, want %d", n, len(over))
	}
	fi, err := os.Stat(filepath.Join(bw.root, "user.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != int64(len(over)) {
		t.Fatalf("user.bin size = %d", fi.Size())
	}

	if _, err := bw.WriteUserUploadStream(ctx, "too.bin", strings.NewReader("0123456789"), 4); !errors.Is(err, ErrUserUploadTooLarge) {
		t.Fatalf("over stream limit: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(bw.root, "too.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("partial upload left on disk: %v", statErr)
	}
}

type repeatByteReader struct{ b byte }

func (r repeatByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}

func TestBotWorkspace_UserStreamAboveDefaultToolCap(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Backend = "local"
	mgr, err := NewBotWorkspaceManager(t.TempDir(), cfg, zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("NewBotWorkspaceManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	ws, err := mgr.GetOrCreate("bot-10mb")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	bw := ws.(*botWorkspace)
	size := int64(cfg.MaxFileWrite) + 1
	ctx := context.Background()

	// 工具写入仍拒绝超过默认 10MB 的缓冲区。用 1 字节越过上限即可，不必再分配 10MB。
	if err := bw.WriteFile(ctx, "tool.bin", make([]byte, cfg.MaxFileWrite+1)); err == nil {
		t.Fatal("default MaxFileWrite accepted a tool write over 10MiB")
	}
	n, err := bw.WriteUserUploadStream(ctx, "user.bin", io.LimitReader(repeatByteReader{b: 'A'}, size), size)
	if err != nil {
		t.Fatalf("user stream above 10MiB: %v", err)
	}
	if n != size {
		t.Fatalf("streamed %d, want %d", n, size)
	}
	fi, err := os.Stat(filepath.Join(bw.root, "user.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != size {
		t.Fatalf("user.bin size = %d, want %d", fi.Size(), size)
	}
}

func TestSandboxWriteFileTool_RejectsOverMaxFileWrite(t *testing.T) {
	mgr, err := NewBotWorkspaceManager(t.TempDir(), Config{
		Backend:      "local",
		MaxFileWrite: 8,
	}, zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("NewBotWorkspaceManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	tool := buildWriteFileTool(mgr, "bot-tool")
	ctx := &llm.ToolExecContext{Context: context.Background()}
	_, err = tool.Execute(ctx, map[string]any{
		"path":    "a.txt",
		"content": "123456789",
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds max write") {
		t.Fatalf("sandbox_write_file over limit: %v", err)
	}
	if _, err := tool.Execute(ctx, map[string]any{
		"path":    "a.txt",
		"content": "12345678",
	}); err != nil {
		t.Fatalf("sandbox_write_file at limit: %v", err)
	}
}
