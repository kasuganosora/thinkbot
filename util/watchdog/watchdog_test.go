package watchdog

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/util/log"
)

// TestMain 初始化日志，避免测试中 log.Logger 为 nil panic。
func TestMain(m *testing.M) {
	_ = log.Init()
	m.Run()
}

// --- 辅助 ---

// waitCtxDone 等待 ctx 被取消，最多等 timeout，返回是否在期内取消。
func waitCtxDone(ctx context.Context, timeout time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(timeout):
		return false
	}
}

// --- 测试用例 ---

// TestTimeoutCancel 验证超时后 context 自动取消。
func TestTimeoutCancel(t *testing.T) {
	wd := New(context.Background(), 50*time.Millisecond)
	defer wd.Stop(true)

	ctx := wd.Context()

	if waitCtxDone(ctx, 200*time.Millisecond) {
		if err := ctx.Err(); !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	} else {
		t.Error("context was not canceled after timeout")
	}
}

// TestFeedPreventsTimeout 验证 Feed 能阻止超时。
func TestFeedPreventsTimeout(t *testing.T) {
	wd := New(context.Background(), 50*time.Millisecond)
	defer wd.Stop(true)

	ctx := wd.Context()

	// 持续投喂 200ms，确保不会被取消
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				wd.Feed()
			case <-stop:
				return
			}
		}
	}()
	defer close(stop)

	if waitCtxDone(ctx, 200*time.Millisecond) {
		t.Error("context was canceled while being fed")
	}
}

// TestFeedThenTimeout 验证停止投喂后会超时取消。
func TestFeedThenTimeout(t *testing.T) {
	wd := New(context.Background(), 50*time.Millisecond)
	defer wd.Stop(true)

	ctx := wd.Context()

	// 先投喂一段时间
	for i := 0; i < 3; i++ {
		wd.Feed()
		time.Sleep(20 * time.Millisecond)
	}

	// 停止投喂，等超时
	if !waitCtxDone(ctx, 200*time.Millisecond) {
		t.Error("context was not canceled after feeding stopped")
	}
}

// TestStopCancelTrue 验证 Stop(true) 取消 context。
func TestStopCancelTrue(t *testing.T) {
	wd := New(context.Background(), 10*time.Second)
	ctx := wd.Context()

	wd.Stop(true)

	if ctx.Err() == nil {
		t.Error("context should be canceled after Stop(true)")
	}
}

// TestStopCancelFalse 验证 Stop(false) 不取消 context。
func TestStopCancelFalse(t *testing.T) {
	wd := New(context.Background(), 10*time.Second)
	ctx := wd.Context()

	wd.Stop(false)

	// 给一点时间确保定时器不会再触发
	time.Sleep(20 * time.Millisecond)
	if ctx.Err() != nil {
		t.Errorf("context should NOT be canceled after Stop(false), got %v", ctx.Err())
	}
}

// TestStopIsIdempotent 验证重复 Stop 不 panic。
func TestStopIsIdempotent(t *testing.T) {
	wd := New(context.Background(), 10*time.Second)

	wd.Stop(true)
	wd.Stop(true) // 不应 panic
}

// TestFeedAfterStop 验证 Stop 后 Feed 不 panic、无副作用。
func TestFeedAfterStop(t *testing.T) {
	wd := New(context.Background(), 10*time.Second)
	wd.Stop(true)

	// 不应 panic
	wd.Feed()
	wd.FeedWithTimeout(5 * time.Second)
}

// TestTimeoutFiresCallback 验证超时触发回调。
func TestTimeoutFiresCallback(t *testing.T) {
	var called atomic.Int32
	wd := NewWithCallback(context.Background(), 30*time.Millisecond, func() {
		called.Add(1)
	})

	time.Sleep(150 * time.Millisecond)
	wd.Stop(true)

	if called.Load() != 1 {
		t.Errorf("expected callback called once, got %d", called.Load())
	}
}

// TestStopPreventsCallback 验证 Stop 能阻止回调。
func TestStopPreventsCallback(t *testing.T) {
	var called atomic.Int32
	wd := NewWithCallback(context.Background(), 50*time.Millisecond, func() {
		called.Add(1)
	})

	wd.Stop(true)
	time.Sleep(150 * time.Millisecond)

	if called.Load() != 0 {
		t.Errorf("callback should NOT fire after Stop, got %d", called.Load())
	}
}

// TestParentCancel 验证 parent 取消后 watchdog context 也会取消。
func TestParentCancel(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	wd := New(parent, 10*time.Second)
	defer wd.Stop(true)

	ctx := wd.Context()

	parentCancel()

	if !waitCtxDone(ctx, 200*time.Millisecond) {
		t.Error("watchdog context should be canceled when parent is canceled")
	}
}

// TestFeedWithTimeoutChangesTimeout 验证 FeedWithTimeout 动态修改超时。
func TestFeedWithTimeoutChangesTimeout(t *testing.T) {
	wd := New(context.Background(), 10*time.Second)
	defer wd.Stop(true)

	original := wd.Timeout()
	if original != 10*time.Second {
		t.Fatalf("expected initial timeout 10s, got %v", original)
	}

	wd.FeedWithTimeout(5 * time.Second)

	if wd.Timeout() != 5*time.Second {
		t.Errorf("expected timeout updated to 5s, got %v", wd.Timeout())
	}
}

// TestFeedWithTimeoutActuallyShortens 验证 FeedWithTimeout 生效后新超时触发取消。
func TestFeedWithTimeoutActuallyShortens(t *testing.T) {
	wd := New(context.Background(), 10*time.Second)
	defer wd.Stop(true)

	wd.FeedWithTimeout(50 * time.Millisecond)
	ctx := wd.Context()

	if !waitCtxDone(ctx, 200*time.Millisecond) {
		t.Error("context should be canceled with shortened timeout")
	}
}

// TestFeedWithTimeoutActuallyLengthens 验证 FeedWithTimeout 能延长超时避免取消。
func TestFeedWithTimeoutActuallyLengthens(t *testing.T) {
	wd := New(context.Background(), 30*time.Millisecond)
	defer wd.Stop(true)

	// 立刻延长到 10 秒
	wd.FeedWithTimeout(10 * time.Second)

	ctx := wd.Context()
	if waitCtxDone(ctx, 200*time.Millisecond) {
		t.Error("context should NOT be canceled with lengthened timeout")
	}
}

// TestName 验证看门狗名称。
func TestName(t *testing.T) {
	wd := NewWithName(context.Background(), 10*time.Second, "test-dog")
	defer wd.Stop(true)

	if wd.Name() != "test-dog" {
		t.Errorf("expected name 'test-dog', got %q", wd.Name())
	}
}

// TestNilParentUsesBackground 验证 parent 为 nil 时不 panic。
func TestNilParentUsesBackground(t *testing.T) {
	wd := New(context.TODO(), 10*time.Second)
	defer wd.Stop(true)

	if wd.Context() == nil {
		t.Error("context should not be nil")
	}
}

// TestContextValid 验证 Context() 返回非 nil 且初始未取消。
func TestContextValid(t *testing.T) {
	wd := New(context.Background(), 10*time.Second)
	defer wd.Stop(true)

	ctx := wd.Context()
	if ctx == nil {
		t.Fatal("context is nil")
	}
	if ctx.Err() != nil {
		t.Errorf("context should not be canceled initially, got %v", ctx.Err())
	}
}

// TestConcurrentFeed 验证并发 Feed 的安全性。
func TestConcurrentFeed(t *testing.T) {
	wd := New(context.Background(), 200*time.Millisecond)
	defer wd.Stop(true)

	done := make(chan struct{})

	// 10 个 goroutine 并发投喂
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 50; j++ {
				wd.Feed()
				time.Sleep(2 * time.Millisecond)
			}
			done <- struct{}{}
		}()
	}

	// 等所有 goroutine 完成
	for i := 0; i < 10; i++ {
		<-done
	}

	// context 应该没被取消
	if wd.Context().Err() != nil {
		t.Error("context should not be canceled during concurrent feeding")
	}
}

// TestTimeoutValueAccessor 验证 Timeout() 返回当前配置值。
func TestTimeoutValueAccessor(t *testing.T) {
	wd := New(context.Background(), 3*time.Second)
	defer wd.Stop(true)

	if got := wd.Timeout(); got != 3*time.Second {
		t.Errorf("expected 3s, got %v", got)
	}
}
