package retry

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/util/log"
)

func TestMain(m *testing.M) {
	_ = log.Init()
	m.Run()
}

// --- 固定间隔 ---

// TestSuccessFirstTry 首次成功不重试。
func TestSuccessFirstTry(t *testing.T) {
	var calls atomic.Int32
	res := Do(context.Background(), "test", Config{
		MaxRetries: 3,
	}, func(ctx context.Context) error {
		calls.Add(1)
		return nil
	})

	if res.Err != nil {
		t.Fatalf("expected nil error, got %v", res.Err)
	}
	if res.Attempts != 1 {
		t.Errorf("expected 1 attempt, got %d", res.Attempts)
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 call, got %d", calls.Load())
	}
}

// TestRetryThenSuccess 重试后成功。
func TestRetryThenSuccess(t *testing.T) {
	var calls atomic.Int32
	res := Do(context.Background(), "test", Config{
		MaxRetries:    3,
		FixedInterval: 5 * time.Millisecond,
	}, func(ctx context.Context) error {
		n := calls.Add(1)
		if n < 3 {
			return errors.New("fail")
		}
		return nil
	})

	if res.Err != nil {
		t.Fatalf("expected success, got %v", res.Err)
	}
	if res.Attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", res.Attempts)
	}
}

// TestRetryExhausted 重试耗尽返回错误。
func TestRetryExhausted(t *testing.T) {
	res := Do(context.Background(), "test", Config{
		MaxRetries:    2,
		FixedInterval: 5 * time.Millisecond,
	}, func(ctx context.Context) error {
		return errors.New("always fail")
	})

	if res.Err == nil {
		t.Fatal("expected error after exhaustion")
	}
	// MaxRetries=2 → 总共执行 3 次（1 初始 + 2 重试）
	if res.Attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", res.Attempts)
	}
}

// TestZeroRetriesNoRetry MaxRetries=0 时不重试。
func TestZeroRetriesNoRetry(t *testing.T) {
	var calls atomic.Int32
	res := Do(context.Background(), "test", Config{
		MaxRetries:    0,
		FixedInterval: 5 * time.Millisecond,
	}, func(ctx context.Context) error {
		calls.Add(1)
		return errors.New("fail")
	})

	if res.Err == nil {
		t.Fatal("expected error")
	}
	if res.Attempts != 1 {
		t.Errorf("expected 1 attempt, got %d", res.Attempts)
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 call, got %d", calls.Load())
	}
}

// TestInfiniteRetry -1 无限重试直到成功。
func TestInfiniteRetry(t *testing.T) {
	var calls atomic.Int32
	res := Do(context.Background(), "test", Config{
		MaxRetries:    -1,
		FixedInterval: 5 * time.Millisecond,
	}, func(ctx context.Context) error {
		n := calls.Add(1)
		if n >= 5 {
			return nil
		}
		return errors.New("fail")
	})

	if res.Err != nil {
		t.Fatalf("expected success, got %v", res.Err)
	}
	if res.Attempts != 5 {
		t.Errorf("expected 5 attempts, got %d", res.Attempts)
	}
}

// TestInfiniteRetryContextCancel 无限重试时 context 取消可中止。
func TestInfiniteRetryContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	res := Do(ctx, "test", Config{
		MaxRetries:    -1,
		FixedInterval: 20 * time.Millisecond,
	}, func(ctx context.Context) error {
		return errors.New("fail")
	})

	if res.Err == nil {
		t.Fatal("expected error due to context cancel")
	}
}

// --- 退避策略 ---

// TestFixedBackoff 固定退避间隔。
func TestFixedBackoff(t *testing.T) {
	b := Backoff{Strategy: StrategyFixed, Initial: 300 * time.Millisecond}

	for i := 1; i <= 5; i++ {
		got := b.Calc(i)
		if got != 300*time.Millisecond {
			t.Errorf("attempt %d: expected 300ms, got %v", i, got)
		}
	}
}

// TestLinearBackoff 线性退避。
func TestLinearBackoff(t *testing.T) {
	b := Backoff{
		Strategy: StrategyLinear,
		Initial:  100 * time.Millisecond,
		Max:      10 * time.Second,
	}

	// attempt 1: 100ms, 2: 200ms, 3: 300ms
	cases := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond}
	for i, want := range cases {
		got := b.Calc(i + 1)
		if got != want {
			t.Errorf("attempt %d: expected %v, got %v", i+1, want, got)
		}
	}
}

// TestExponentialBackoff 指数退避。
func TestExponentialBackoff(t *testing.T) {
	b := Backoff{
		Strategy: StrategyExponential,
		Initial:  100 * time.Millisecond,
		Factor:   2.0,
		Max:      10 * time.Second,
	}

	// 100ms, 200ms, 400ms, 800ms
	cases := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
	}
	for i, want := range cases {
		got := b.Calc(i + 1)
		if got != want {
			t.Errorf("attempt %d: expected %v, got %v", i+1, want, got)
		}
	}
}

// TestExponentialBackoffMaxCap 指数退避上限。
func TestExponentialBackoffMaxCap(t *testing.T) {
	b := Backoff{
		Strategy: StrategyExponential,
		Initial:  100 * time.Millisecond,
		Factor:   2.0,
		Max:      500 * time.Millisecond,
	}

	// attempt 4: 800ms → cap to 500ms
	got := b.Calc(4)
	if got != 500*time.Millisecond {
		t.Errorf("expected capped to 500ms, got %v", got)
	}
}

// TestBackoffWithJitter 退避抖动在合理范围内。
func TestBackoffWithJitter(t *testing.T) {
	b := Backoff{
		Strategy: StrategyFixed,
		Initial:  100 * time.Millisecond,
		Jitter:   true,
	}

	for i := 1; i <= 20; i++ {
		got := b.Calc(i)
		// jitter range: [50ms, 150ms)
		if got < 50*time.Millisecond || got >= 150*time.Millisecond {
			t.Errorf("attempt %d: jittered wait %v out of [50ms, 150ms)", i, got)
		}
	}
}

// TestBackoffActuallyUsed 验证 Backoff 配置确实被使用。
func TestBackoffActuallyUsed(t *testing.T) {
	start := time.Now()
	var waits []time.Duration

	res := Do(context.Background(), "test", Config{
		MaxRetries: 2,
		Backoff: &Backoff{
			Strategy: StrategyFixed,
			Initial:  100 * time.Millisecond,
		},
		OnRetry: func(attempt int, err error, wait time.Duration) {
			waits = append(waits, wait)
		},
	}, func(ctx context.Context) error {
		return errors.New("fail")
	})

	_ = res
	elapsed := time.Since(start)

	// 2 次重试 × 100ms = 至少 200ms
	if elapsed < 190*time.Millisecond {
		t.Errorf("expected >=200ms elapsed (2×100ms backoff), got %v", elapsed)
	}
	if len(waits) != 2 {
		t.Errorf("expected 2 retry callbacks, got %d", len(waits))
	}
	for i, w := range waits {
		if w != 100*time.Millisecond {
			t.Errorf("wait %d: expected 100ms, got %v", i, w)
		}
	}
}

// --- Panic 恢复 ---

// TestPanicRecoveredThenSuccess panic 后继续重试直到成功。
func TestPanicRecoveredThenSuccess(t *testing.T) {
	var calls atomic.Int32
	res := Do(context.Background(), "test", Config{
		MaxRetries:    3,
		FixedInterval: 5 * time.Millisecond,
	}, func(ctx context.Context) error {
		n := calls.Add(1)
		if n == 1 {
			panic("boom")
		}
		return nil
	})

	if res.Err != nil {
		t.Fatalf("expected success, got %v", res.Err)
	}
	if res.Attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", res.Attempts)
	}
	if res.Panics != 1 {
		t.Errorf("expected 1 panic, got %d", res.Panics)
	}
}

// TestPanicExhausted panic 后重试耗尽。
func TestPanicExhausted(t *testing.T) {
	res := Do(context.Background(), "test", Config{
		MaxRetries:    2,
		FixedInterval: 5 * time.Millisecond,
	}, func(ctx context.Context) error {
		panic("always boom")
	})

	if res.Err == nil {
		t.Fatal("expected error")
	}
	if res.Panics != 3 {
		t.Errorf("expected 3 panics, got %d", res.Panics)
	}
}

// TestPanicOnPanicCallback 验证 OnPanic 回调被调用。
func TestPanicOnPanicCallback(t *testing.T) {
	var panicValues []interface{}

	Do(context.Background(), "test", Config{
		MaxRetries:    1,
		FixedInterval: 5 * time.Millisecond,
		OnPanic: func(attempt int, r interface{}, stack []byte) {
			panicValues = append(panicValues, r)
		},
	}, func(ctx context.Context) error {
		panic("callback test")
	})

	if len(panicValues) != 2 {
		t.Errorf("expected 2 panic callbacks, got %d", len(panicValues))
	}
}

// TestPanicNoRecover RecoverPanic=false 时 panic 直接传播。
func TestPanicNoRecover(t *testing.T) {
	recoverPanic := false

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic to propagate")
		}
	}()

	Do(context.Background(), "test", Config{
		MaxRetries:    3,
		FixedInterval: 5 * time.Millisecond,
		RecoverPanic:  &recoverPanic,
	}, func(ctx context.Context) error {
		panic("should propagate")
	})
}

// --- Context 取消 ---

// TestContextCancelDuringWait 在等待退避期间 context 被取消。
func TestContextCancelDuringWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	res := Do(ctx, "test", Config{
		MaxRetries:    -1,
		FixedInterval: 100 * time.Millisecond,
	}, func(ctx context.Context) error {
		return errors.New("fail")
	})

	if res.Err == nil {
		t.Fatal("expected error from context cancel")
	}
}

// --- 回调 ---

// TestOnRetryCallback 验证 OnRetry 回调参数。
func TestOnRetryCallback(t *testing.T) {
	type record struct {
		attempt int
		wait    time.Duration
	}
	var records []record

	Do(context.Background(), "test", Config{
		MaxRetries:    2,
		FixedInterval: 10 * time.Millisecond,
		OnRetry: func(attempt int, err error, wait time.Duration) {
			records = append(records, record{attempt, wait})
		},
	}, func(ctx context.Context) error {
		return fmt.Errorf("err-%d", attempt(ctx))
	})

	if len(records) != 2 {
		t.Fatalf("expected 2 retry records, got %d", len(records))
	}
	if records[0].attempt != 1 || records[1].attempt != 2 {
		t.Errorf("unexpected attempts: %+v", records)
	}
	if records[0].wait != 10*time.Millisecond {
		t.Errorf("unexpected wait: %v", records[0].wait)
	}
}

// attempt 辅助：从 ctx 提取 attempt（这里仅用于生成不同错误消息，不影响逻辑）
func attempt(ctx context.Context) int { return 0 }

// --- DoSimple ---

// TestDoSimple 验证 DoSimple 快捷方法。
func TestDoSimple(t *testing.T) {
	var calls atomic.Int32
	err := DoSimple(context.Background(), "test", func(ctx context.Context) error {
		calls.Add(1)
		return nil
	})

	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 call, got %d", calls.Load())
	}
}
