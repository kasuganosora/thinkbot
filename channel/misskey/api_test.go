package misskey

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

// misskeyErrorServer 返回一个总是以指定状态码 + 错误码响应的测试服务器，
// 用于复现真实实例对幂等冲突的响应。
func misskeyErrorServer(t *testing.T, code string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"conflict","code":"` + code + `","kind":"client"}}`))
	}))
}

// TestCreateReaction_AlreadyReacted 锁定 2026-08-30 的生产缺陷：
// ALREADY_REACTED 的幂等判定原先只写在 resp.StatusCode 分支上，而本项目的
// HTTP 客户端把 4xx 直接作为 err 从 Do() 返回，该分支永远走不到——
// 修复上线后当日仍有 2 次 400 被上报为工具失败。
func TestCreateReaction_AlreadyReacted(t *testing.T) {
	srv := misskeyErrorServer(t, "ALREADY_REACTED")
	defer srv.Close()

	a := newAPIClient(srv.URL, "tok")
	err := a.createReaction(context.Background(), "note-1", "👍")
	if !errors.Is(err, ErrAlreadyReacted) {
		t.Fatalf("期望 ErrAlreadyReacted（幂等成功），实际: %v", err)
	}
}

// TestDeleteReaction_NotReacted 与 createReaction 对称：撤销一个不存在的反应
// 属幂等成功，不应上报为失败。
func TestDeleteReaction_NotReacted(t *testing.T) {
	srv := misskeyErrorServer(t, "NOT_REACTED")
	defer srv.Close()

	a := newAPIClient(srv.URL, "tok")
	err := a.deleteReaction(context.Background(), "note-1")
	if !errors.Is(err, ErrNotReacted) {
		t.Fatalf("期望 ErrNotReacted（幂等成功），实际: %v", err)
	}
}

// TestReaction_OtherErrorsStillSurface 非幂等错误必须照常上报，
// 不能因为加了幂等判定而被吞掉。
func TestReaction_OtherErrorsStillSurface(t *testing.T) {
	srv := misskeyErrorServer(t, "NO_SUCH_NOTE")
	defer srv.Close()

	a := newAPIClient(srv.URL, "tok")
	if err := a.createReaction(context.Background(), "note-1", "👍"); errors.Is(err, ErrAlreadyReacted) {
		t.Fatal("非 ALREADY_REACTED 错误不应被判为幂等成功")
	}
	if err := a.deleteReaction(context.Background(), "note-1"); errors.Is(err, ErrNotReacted) {
		t.Fatal("非 NOT_REACTED 错误不应被判为幂等成功")
	}
}

// TestErrHasMisskeyCode 校验错误码识别辅助函数。
func TestErrHasMisskeyCode(t *testing.T) {
	if errHasMisskeyCode(nil, "ALREADY_REACTED") {
		t.Error("nil error 不应匹配任何错误码")
	}
	if !errHasMisskeyCode(errors.New(`-> 400: {"error":{"code":"ALREADY_REACTED"}}`), "ALREADY_REACTED") {
		t.Error("应能从错误文本中识别出错误码")
	}
	if errHasMisskeyCode(errors.New(`-> 400: {"error":{"code":"OTHER"}}`), "ALREADY_REACTED") {
		t.Error("不应误匹配其他错误码")
	}
}
