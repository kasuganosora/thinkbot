package bot

import (
	"testing"
	"time"
)

// TestLLMClientTimeout_LongEnoughForNonStreaming 守住 LLM HTTP 客户端超时的下限。
//
// 事故（2026-08-03）：该值为 5 分钟时，workflow 节点执行连续 3 次尝试全部精准卡在
// 5 分钟超时（17:01 / 17:06 / 17:12），报
//
//	Post ".../chat/completions": context deadline exceeded
//	(Client.Timeout exceeded while awaiting headers)
//
// 于是本可完成的节点被判失败，并级联 skip 掉全部下游节点。
//
// 原因：这是**整个请求**的上限（含等待首字节）。非流式请求（llm.OrchestrateGenerate，
// workflow 的 SubAgent 走这条路）必须等模型生成完整段回复才返回响应头，「写大量代码 /
// 长篇审查报告」这类调用轻易超过数分钟。而 SSE 看门狗**只保护流式路径**，
// 对非流式不生效，所以不能靠它兜底把超时压短。
func TestLLMClientTimeout_LongEnoughForNonStreaming(t *testing.T) {
	const minSane = 10 * time.Minute
	if llmClientTimeout < minSane {
		t.Fatalf("llmClientTimeout = %v, want >= %v; long non-streaming generations "+
			"(workflow SubAgent code review/fix) would be cut off and fail whole workflows",
			llmClientTimeout, minSane)
	}
	// 上限守卫：真正挂死的请求也不该无限占用连接。
	const maxSane = time.Hour
	if llmClientTimeout > maxSane {
		t.Fatalf("llmClientTimeout = %v, want <= %v; a genuinely hung request would "+
			"hold the connection far too long", llmClientTimeout, maxSane)
	}
}
