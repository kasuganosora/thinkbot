package sandbox

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
)

// TestConcurrentReplaceSameFileKeepsBothEdits 锁定并行工作流节点同时改同一文件：
// 两次 replace 各改一行，两次修改都必须留下。没有按路径加锁时，两边都读到
// 原始内容再写回，后写的那次把先写的改动盖掉。
func TestConcurrentReplaceSameFileKeepsBothEdits(t *testing.T) {
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	ws := getBotWS(t, mgr, botID)
	if err := ws.WriteFile(context.Background(), "shared.txt", []byte("alpha\nbeta\n")); err != nil {
		t.Fatal(err)
	}

	// 两边都读完、都还没写时再放行，把丢失更新变成必现。
	var mu sync.Mutex
	var readers int
	release := make(chan struct{})
	var once sync.Once
	testAfterWorkspaceRead = func(string) {
		mu.Lock()
		readers++
		n := readers
		mu.Unlock()
		if n >= 2 {
			once.Do(func() { close(release) })
			return
		}
		select {
		case <-release:
		case <-time.After(300 * time.Millisecond):
		}
	}
	t.Cleanup(func() { testAfterWorkspaceRead = nil })

	tool := buildReplaceInFileTool(mgr, botID)
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, old := range []string{"alpha", "beta"} {
		wg.Add(1)
		go func(old string) {
			defer wg.Done()
			_, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{
				"path":    "shared.txt",
				"old_str": old,
				"new_str": old + "-done",
			})
			errCh <- err
		}(old)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("replace: %v", err)
		}
	}

	data, err := ws.ReadFile(context.Background(), "shared.txt")
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "alpha-done") || !strings.Contains(got, "beta-done") {
		t.Fatalf("concurrent replace lost an edit:\n%s", got)
	}
}

// TestConcurrentReplaceDifferentFilesOverlap 锁定锁的粒度是路径而不是全局：
// 两个不同文件的 replace 必须能同时处于读和写之间。
func TestConcurrentReplaceDifferentFilesOverlap(t *testing.T) {
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	ws := getBotWS(t, mgr, botID)
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := ws.WriteFile(context.Background(), name, []byte("line\n")); err != nil {
			t.Fatal(err)
		}
	}

	entered := make(chan string, 2)
	release := make(chan struct{})
	testAfterWorkspaceRead = func(p string) {
		entered <- p
		<-release
	}
	t.Cleanup(func() { testAfterWorkspaceRead = nil })

	tool := buildReplaceInFileTool(mgr, botID)
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, name := range []string{"a.txt", "b.txt"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			_, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{
				"path":    name,
				"old_str": "line",
				"new_str": "line-done",
			})
			errCh <- err
		}(name)
	}

	got := map[string]bool{}
	timeout := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case p := <-entered:
			got[p] = true
		case <-timeout:
			close(release)
			wg.Wait()
			t.Fatalf("different files did not overlap inside replace, entered=%v", got)
		}
	}
	close(release)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("replace: %v", err)
		}
	}
}
