package api

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// TestPureRenoteEnrichFn 是 pure-renote enricher 逻辑体的回归测试。
// 它必须：当入站消息被标记为纯 Renote（core.MetaIsPureRenote=true）时，
// 设硬权限抑制（KVSuppressReply + KVSuppressReasonPureRenote），
// 且原因必须是硬门（IsHardSuppressReason=true，不可被模型 REPLY_CONTROL send:true 覆盖）。
// 其余情况（metadata 为 nil、标记为 false）必须保持 no-op，不动 suppress 状态。
func TestPureRenoteEnrichFn(t *testing.T) {
	t.Run("纯 Renote 标记 -> 硬抑制", func(t *testing.T) {
		env := core.NewEnvelope(core.Message{
			Metadata: map[string]any{core.MetaIsPureRenote: true},
		})
		if err := pureRenoteEnrichFn(context.Background(), env); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		suppressed, ok := env.Get(core.KVSuppressReply)
		if !ok || suppressed != true {
			t.Fatalf("expected KVSuppressReply=true, got ok=%v val=%v", ok, suppressed)
		}
		reason, _ := env.Get(core.KVSuppressReplyReason)
		if reason != core.KVSuppressReasonPureRenote {
			t.Fatalf("expected reason %q, got %v", core.KVSuppressReasonPureRenote, reason)
		}
		if !core.IsHardSuppressReason(reason) {
			t.Fatalf("pure-renote reason must be a hard suppress reason (cannot be overridden by model)")
		}
	})

	t.Run("非纯 Renote（标记 false）-> no-op", func(t *testing.T) {
		env := core.NewEnvelope(core.Message{
			Metadata: map[string]any{core.MetaIsPureRenote: false},
		})
		if err := pureRenoteEnrichFn(context.Background(), env); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := env.Get(core.KVSuppressReply); ok {
			t.Fatalf("expected no KVSuppressReply set for non-pure-renote")
		}
	})

	t.Run("metadata 为 nil -> no-op 不 panic", func(t *testing.T) {
		env := core.NewEnvelope(core.Message{})
		if err := pureRenoteEnrichFn(context.Background(), env); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := env.Get(core.KVSuppressReply); ok {
			t.Fatalf("expected no KVSuppressReply set when metadata is nil")
		}
	})

	t.Run("硬抑制不可被 REPLY_CONTROL 覆盖的契约", func(t *testing.T) {
		// 确保原因落在 IsHardSuppressReason 的硬门集合内：
		// 下游 llmroute 在 REPLY_CONTROL send:true 时仍须尊重硬门，不发起回复请求。
		if !core.IsHardSuppressReason(core.KVSuppressReasonPureRenote) {
			t.Fatalf("%q 必须被 IsHardSuppressReason 判定为硬门", core.KVSuppressReasonPureRenote)
		}
	})
}
