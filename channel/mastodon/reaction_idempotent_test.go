package mastodon

// 表态幂等哨兵测试。
//
// 锁定 2026-10-19 真实实例（c.hime.at 4.3.0+community）实测推翻的错误前提：
// 原先代码与 README 都断言「Mastodon 重复表态天然幂等，返回 200，不需要
// Misskey 那套哨兵错误」。实测四个端点**全部**在重复/无效时返回 400。
// 上游 Mastodon 确实返回 200，所以修法必须同时兼容两种行为。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mastodonErrorServer 起一个恒定返回 400 + {"error":<msg>} 的测试服务端。
// Mastodon 的错误响应体是 `{"error":"..."}`（不是 Misskey 的嵌套 error.code）。
func mastodonErrorServer(t *testing.T, msg string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
	}))
}

func TestReactionIdempotentSentinels(t *testing.T) {
	cases := []struct {
		name     string
		msg      string
		call     func(a *apiClient) error
		wantSent error
	}{
		{
			name:     "重复收藏",
			msg:      "already favourited",
			call:     func(a *apiClient) error { return a.favourite(context.Background(), "1") },
			wantSent: ErrAlreadyFavourited,
		},
		{
			name:     "取消不存在的收藏",
			msg:      "not favourited",
			call:     func(a *apiClient) error { return a.unfavourite(context.Background(), "1") },
			wantSent: ErrNotFavourited,
		},
		{
			name:     "重复转发",
			msg:      "already reblogged",
			call:     func(a *apiClient) error { return a.reblog(context.Background(), "1") },
			wantSent: ErrAlreadyReblogged,
		},
		{
			name:     "撤销不存在的转发",
			msg:      "not reblogged",
			call:     func(a *apiClient) error { return a.unreblog(context.Background(), "1") },
			wantSent: ErrNotReblogged,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := mastodonErrorServer(t, tc.msg)
			defer srv.Close()
			a := newAPIClient(srv.URL, "tok")
			err := tc.call(a)
			if !errors.Is(err, tc.wantSent) {
				t.Fatalf("期望哨兵 %v，实际: %v", tc.wantSent, err)
			}
		})
	}
}

// TestReactionOtherErrorNotSwallowed 反向验证：其它 400 不能被误判成幂等成功。
func TestReactionOtherErrorNotSwallowed(t *testing.T) {
	srv := mastodonErrorServer(t, "record not found")
	defer srv.Close()
	a := newAPIClient(srv.URL, "tok")
	err := a.favourite(context.Background(), "1")
	if err == nil {
		t.Fatal("非幂等错误不应被吞掉")
	}
	for _, sent := range []error{ErrAlreadyFavourited, ErrNotFavourited, ErrAlreadyReblogged, ErrNotReblogged} {
		if errors.Is(err, sent) {
			t.Fatalf("非幂等错误被误判为 %v: %v", sent, err)
		}
	}
}

// TestErrHasMastodonError 覆盖文本匹配的边界（大小写、nil、空串）。
func TestErrHasMastodonError(t *testing.T) {
	if errHasMastodonError(nil, "already favourited") {
		t.Error("nil err 不应匹配")
	}
	if errHasMastodonError(errors.New(`-> 400: {"error":"already favourited"}`), "") {
		t.Error("空 msg 不应匹配")
	}
	if !errHasMastodonError(errors.New(`-> 400: {"error":"already favourited"}`), "already favourited") {
		t.Error("精确文本应匹配")
	}
	// 大小写不敏感：不同实例/版本文案大小写可能不同
	if !errHasMastodonError(errors.New(`-> 400: {"error":"Already Favourited"}`), "already favourited") {
		t.Error("大小写差异应匹配")
	}
	if errHasMastodonError(errors.New(`-> 400: {"error":"already reblogged"}`), "already favourited") {
		t.Error("不同文案不应互相匹配")
	}
}

// TestChannelReactionSwallowsSentinel 验证 channel 层把幂等哨兵吞成 nil
// （上层是出站 Action 处理器，不该看到「已经做过了」这种伪错误）。
func TestChannelReactionSwallowsSentinel(t *testing.T) {
	// 转发：重复 boost
	srv := mastodonErrorServer(t, "already reblogged")
	defer srv.Close()
	c := &MastodonChannel{name: "t", cfg: Config{Server: srv.URL, Token: "tok"}}
	c.api = newAPIClient(srv.URL, "tok")
	if err := c.Boost(context.Background(), "1"); err != nil {
		t.Fatalf("Boost 重复应被吞成 nil，实际: %v", err)
	}

	// 收藏：重复 favourite
	srv2 := mastodonErrorServer(t, "already favourited")
	defer srv2.Close()
	c2 := &MastodonChannel{name: "t", cfg: Config{Server: srv2.URL, Token: "tok"}}
	c2.api = newAPIClient(srv2.URL, "tok")
	if err := c2.Favourite(context.Background(), "1"); err != nil {
		t.Fatalf("Favourite 重复应被吞成 nil，实际: %v", err)
	}

	// 反向：非幂等错误必须继续向上抛
	srv3 := mastodonErrorServer(t, "record not found")
	defer srv3.Close()
	c3 := &MastodonChannel{name: "t", cfg: Config{Server: srv3.URL, Token: "tok"}}
	c3.api = newAPIClient(srv3.URL, "tok")
	if err := c3.Boost(context.Background(), "1"); err == nil {
		t.Fatal("非幂等错误不应被吞掉")
	}
}
