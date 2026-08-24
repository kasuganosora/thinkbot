package inbound

import (
	"context"
	"sync"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// Ingress 消息去重回归测试
//
// 背景：Misskey 上同一条帖子曾被 Bot 回复 2~4 次。根因是同一条入站消息被多个
// 来源重复投递（channel 的 main 事件 + 多个 timeline 通道、WS 重连重放、
// 以及部署期多实例并存），每次投递都独立跑一条 pipeline 并各自产出回复。
//
// Ingress 是每个 Bot 所有入站消息的唯一单例汇聚点，因此在此按 msg.ID 去重是
// 最后一道、也是最可靠的一道防线。以下测试锁定该行为，防止后续重构静默破坏。
// ============================================================================

// TestIngress_ReceiveDedupSameMessageID 复现「同一条帖子被多个事件源重复投递」：
// 同一 msg.ID 投递 4 次（对应线上观测到的 4 条并发 pipeline），只应有 1 条进入队列。
func TestIngress_ReceiveDedupSameMessageID(t *testing.T) {
	g := testIngress(16)
	defer g.Close()

	ctx := context.Background()
	const noteID = "note-aqailbnedi7a0126"

	for i := 0; i < 4; i++ {
		if err := g.Receive(ctx, core.Message{
			ID:      noteID,
			Source:  "misskey",
			Channel: "misskey:main",
			Text:    "同一条帖子的重复投递",
		}); err != nil {
			t.Fatalf("Receive #%d 返回错误: %v", i+1, err)
		}
	}

	if got := g.Len(); got != 1 {
		t.Fatalf("同一 msg.ID 投递 4 次后期望队列中只有 1 条，实际 %d 条（去重失效会导致重复回复）", got)
	}
}

// TestIngress_ReceiveDedupAcrossChannels 确认去重是跨通道的：
// 同一 noteID 分别以 main / timeline 身份到达时，仍只处理一次。
// 这正是线上重复的主要来源，per-channel 隔离的去重挡不住。
func TestIngress_ReceiveDedupAcrossChannels(t *testing.T) {
	g := testIngress(16)
	defer g.Close()

	ctx := context.Background()
	const noteID = "note-cross-channel"

	for _, ch := range []string{"misskey:main", "misskey:tl:homeTimeline", "misskey:tl:localTimeline"} {
		if err := g.Receive(ctx, core.Message{
			ID:      noteID,
			Source:  "misskey",
			Channel: ch,
		}); err != nil {
			t.Fatalf("Receive(channel=%s) 返回错误: %v", ch, err)
		}
	}

	if got := g.Len(); got != 1 {
		t.Fatalf("同一 noteID 经 3 个通道到达后期望只处理 1 次，实际 %d 次", got)
	}
}

// TestIngress_ReceiveDedupConcurrent 验证 seen() 的「检查并设置」是原子的。
// 线上现象正是多个 goroutine 几乎同时处理同一条消息，若去重有竞态则会漏过多条。
func TestIngress_ReceiveDedupConcurrent(t *testing.T) {
	g := testIngress(128)
	defer g.Close()

	const noteID = "note-concurrent"
	const n = 32

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = g.Receive(context.Background(), core.Message{
				ID:     noteID,
				Source: "misskey",
			})
		}()
	}
	wg.Wait()

	if got := g.Len(); got != 1 {
		t.Fatalf("%d 个 goroutine 并发投递同一 msg.ID 后期望只有 1 条入队，实际 %d 条（存在竞态）", n, got)
	}
}

// TestIngress_ReceiveDistinctIDsNotDropped 保证去重不会误杀正常消息：
// 不同 msg.ID 必须全部放行，否则会出现「回复了 Bot 却没反应」。
func TestIngress_ReceiveDistinctIDsNotDropped(t *testing.T) {
	g := testIngress(16)
	defer g.Close()

	ctx := context.Background()
	ids := []string{"note-a", "note-b", "note-c"}

	for _, id := range ids {
		if err := g.Receive(ctx, core.Message{ID: id, Source: "misskey"}); err != nil {
			t.Fatalf("Receive(id=%s) 返回错误: %v", id, err)
		}
	}

	if got := g.Len(); got != len(ids) {
		t.Fatalf("%d 条不同消息期望全部入队，实际 %d 条（去重误杀）", len(ids), got)
	}
}

// TestIngress_ReceiveEmptyIDNotDeduped 保证无 ID 的消息不被互相去重。
// 无 ID 消息由 Ingress 自动补唯一 ID，彼此独立，必须全部放行。
func TestIngress_ReceiveEmptyIDNotDeduped(t *testing.T) {
	g := testIngress(16)
	defer g.Close()

	ctx := context.Background()
	const n = 3

	for i := 0; i < n; i++ {
		if err := g.Receive(ctx, core.Message{Source: "web", Text: "无 ID 消息"}); err != nil {
			t.Fatalf("Receive #%d 返回错误: %v", i+1, err)
		}
	}

	if got := g.Len(); got != n {
		t.Fatalf("%d 条无 ID 消息期望全部入队，实际 %d 条", n, got)
	}
}

// TestIngress_TryReceiveDedup 确认非阻塞入口 TryReceive 同样受去重保护，
// 避免两个入口行为不一致留下绕过路径。
func TestIngress_TryReceiveDedup(t *testing.T) {
	g := testIngress(16)
	defer g.Close()

	const noteID = "note-tryreceive"

	for i := 0; i < 3; i++ {
		if ok := g.TryReceive(core.Message{ID: noteID, Source: "misskey"}); !ok {
			t.Fatalf("TryReceive #%d 期望返回 true（已处理/已去重），实际 false", i+1)
		}
	}

	if got := g.Len(); got != 1 {
		t.Fatalf("TryReceive 同一 msg.ID 投递 3 次后期望只有 1 条入队，实际 %d 条", got)
	}
}
