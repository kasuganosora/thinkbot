package misskey

import (
	"errors"
	"testing"
	"time"
)

// TestSearchBreakerClosedByDefault 初始状态必须放行。
func TestSearchBreakerClosedByDefault(t *testing.T) {
	b := &searchBreaker{}
	if ok, _ := b.allow(time.Now()); !ok {
		t.Fatal("初始状态应放行")
	}
}

// TestSearchBreakerOpensAfterThreshold 连续失败达阈值后进入熔断，冷却期内不放行。
func TestSearchBreakerOpensAfterThreshold(t *testing.T) {
	b := &searchBreaker{}
	now := time.Now()

	opened := b.recordFailure(now, "HTTP 500")
	if opened {
		t.Fatalf("第 1 次失败不应触发熔断（阈值 %d）", searchBreakerThreshold)
	}
	if ok, _ := b.allow(now.Add(time.Second)); !ok {
		t.Fatal("未达阈值时不应熔断")
	}

	opened = b.recordFailure(now.Add(time.Second), "HTTP 500")
	if !opened {
		t.Fatal("达到阈值后应触发熔断")
	}
	if ok, _ := b.allow(now.Add(time.Second)); ok {
		t.Fatal("熔断期内不应放行")
	}
}

// TestSearchBreakerReopensAfterCooldown 冷却结束后恢复放行（半开试探）。
func TestSearchBreakerReopensAfterCooldown(t *testing.T) {
	b := &searchBreaker{}
	now := time.Now()
	b.recordFailure(now, "HTTP 500")
	b.recordFailure(now, "HTTP 500")

	after := now.Add(searchBreakerCooldown + time.Second)
	if ok, _ := b.allow(after); !ok {
		t.Fatal("冷却结束后应恢复放行")
	}
}

// TestSearchBreakerSuccessResets 成功重置计数，后续单次失败不会立刻熔断。
func TestSearchBreakerSuccessResets(t *testing.T) {
	b := &searchBreaker{}
	now := time.Now()
	b.recordFailure(now, "HTTP 500")
	b.recordSuccess()
	if opened := b.recordFailure(now.Add(time.Second), "HTTP 500"); opened {
		t.Fatal("成功重置后，单次失败不应触发熔断")
	}
}

// TestSearchBreakerHalfOpenFailureReopens 冷却结束放行试探，试探失败立即重新熔断。
func TestSearchBreakerHalfOpenFailureReopens(t *testing.T) {
	b := &searchBreaker{}
	now := time.Now()
	b.recordFailure(now, "HTTP 500")
	b.recordFailure(now, "HTTP 500") // 熔断至 now+cooldown

	probe := now.Add(searchBreakerCooldown + time.Second)
	if ok, _ := b.allow(probe); !ok {
		t.Fatal("冷却结束后应放行试探请求")
	}
	if opened := b.recordFailure(probe, "HTTP 500"); !opened {
		t.Fatal("半开试探失败应立即重新熔断")
	}
	if ok, _ := b.allow(probe.Add(time.Second)); ok {
		t.Fatal("重新熔断后不应放行")
	}
}

// TestSearchBreakerHalfOpenSuccessCloses 试探成功应彻底关闭熔断。
func TestSearchBreakerHalfOpenSuccessCloses(t *testing.T) {
	b := &searchBreaker{}
	now := time.Now()
	b.recordFailure(now, "HTTP 500")
	b.recordFailure(now, "HTTP 500")

	probe := now.Add(searchBreakerCooldown + time.Second)
	b.allow(probe)
	b.recordSuccess()

	// 关闭后单次失败不应重新熔断。
	if opened := b.recordFailure(probe.Add(time.Second), "HTTP 500"); opened {
		t.Fatal("熔断关闭后，单次失败不应触发熔断")
	}
	if ok, _ := b.allow(probe.Add(2 * time.Second)); !ok {
		t.Fatal("试探成功后应持续放行")
	}
}

// TestErrSearchUnavailable 错误哨兵可被 errors.Is 识别（工具层据此降级）。
func TestErrSearchUnavailable(t *testing.T) {
	wrapped := errors.Join(errors.New("outer"), ErrSearchUnavailable)
	if !errors.Is(wrapped, ErrSearchUnavailable) {
		t.Fatal("ErrSearchUnavailable 应可被 errors.Is 识别")
	}
}
