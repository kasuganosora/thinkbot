package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/config"
)

// TestBotFileUpload_RealFilesystem 验证文件上传真实落盘、列目录真实读盘、防目录穿越。
// 这是对「文件管理非 mock、真实读写 {workspace}/{botID}/」的运行时证明。
func TestBotFileUpload_RealFilesystem(t *testing.T) {
	tmp := t.TempDir()

	// store（db=nil 时 Set 走内存覆盖），注入工作空间根目录到临时目录。
	store := config.NewStore(nil)
	if err := store.Set(context.TODO(), config.KeyWorkspaceDir, tmp); err != nil {
		t.Fatalf("set workspace dir: %v", err)
	}

	s := &Server{
		store:  store,
		logger: zap.NewNop().Sugar(),
	}

	const botID = "bot-test-123"

	// 1. 上传文件到根目录
	content := "hello real filesystem upload"
	if err := s.botFileUpload(nil, botID, "/", "note.txt", strings.NewReader(content)); err != nil {
		t.Fatalf("upload to root: %v", err)
	}

	// 断言：文件真实存在于磁盘 {tmp}/{botID}/note.txt，且内容正确
	diskPath := filepath.Join(tmp, botID, "note.txt")
	got, err := os.ReadFile(diskPath)
	if err != nil {
		t.Fatalf("read back uploaded file from disk: %v", err)
	}
	if string(got) != content {
		t.Fatalf("content mismatch: got %q want %q", string(got), content)
	}

	// 2. 上传到子目录（目录不存在时自动创建）
	if err := s.botFileUpload(nil, botID, "/sub/deep", "a.log", strings.NewReader("x")); err != nil {
		t.Fatalf("upload to nested dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, botID, "sub", "deep", "a.log")); err != nil {
		t.Fatalf("nested file not on disk: %v", err)
	}

	// 3. 列根目录，应真实读回 note.txt（文件）和 sub（目录）
	entries := s.getBotFileEntries(botID, "/")
	var sawFile, sawDir bool
	for _, e := range entries {
		if e.Name == "note.txt" && e.Type == "file" {
			sawFile = true
			if e.Size != int64(len(content)) {
				t.Fatalf("size mismatch: got %d want %d", e.Size, len(content))
			}
		}
		if e.Name == "sub" && e.Type == "dir" {
			sawDir = true
		}
	}
	if !sawFile || !sawDir {
		t.Fatalf("list entries incorrect: %+v (sawFile=%v sawDir=%v)", entries, sawFile, sawDir)
	}

	// 4. mkdir 真实建目录
	if err := s.botFileMkdir(nil, botID, "/", "mydir"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(tmp, botID, "mydir")); err != nil || !fi.IsDir() {
		t.Fatalf("mkdir not on disk: %v", err)
	}

	// 5. 防目录穿越：上传/列目录携带 ../ 必须被拒绝或限制在根内，不得逃逸
	escapeMarker := filepath.Join(tmp, "escaped.txt")
	if err := s.botFileUpload(nil, botID, "/../../..", "escaped.txt", strings.NewReader("evil")); err == nil {
		// 若未报错，也必须确保没有真的写到工作根之外
		if _, statErr := os.Stat(escapeMarker); statErr == nil {
			t.Fatalf("directory traversal succeeded! file escaped to %s", escapeMarker)
		}
	}
	if _, statErr := os.Stat(escapeMarker); statErr == nil {
		t.Fatalf("directory traversal wrote outside workspace: %s", escapeMarker)
	}

	// 6. 非法文件名拒绝
	if err := s.botFileUpload(nil, botID, "/", "..", strings.NewReader("x")); err == nil {
		t.Fatalf("expected invalid file name rejected")
	}

	t.Logf("OK: uploads landed under %s", filepath.Join(tmp, botID))
}
