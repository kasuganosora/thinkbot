package api

import (
	"encoding/json"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/internal/interaction"
	"github.com/kasuganosora/thinkbot/tools"
)

// TestUserChoiceSSEFlow 端到端复现后端 user_choice 进度事件 → 前端 extractChoicePayload 的全过程，
// 锁定"卡片没有选项"的根因在前端还是后端。
//
// 数据流：
//  1. 工具调用 ctx.SendProgress(ev)（ev 为 UserChoiceEventPayload 结构体）→
//     事件总线把 ev 原样存入 event.Data["payload"]；
//  2. translateEventToSSE 把事件翻译成 SSE 的 map（payload 字段原样透传）；
//  3. SSE 写入端把 map JSON 序列化（模拟）；
//  4. 前端 services.js 解析 JSON，并把 parts.payload 展开成 payload（补 stream/chunk）；
//  5. 前端 extractChoicePayload 形态三：payload.type==='user_choice' 且 questionId!=null →
//     归一化 → registerChoice → ChoiceCard 渲染 options。
//
// 若本测试 FAIL，说明后端序列化出的 payload 在到前端 extractChoicePayload 之前就丢了
// type / questionId / options[].id，前端必然拿不到选项。
func TestUserChoiceSSEFlow(t *testing.T) {
	ev := tools.UserChoiceEventPayload{
		Type:       "user_choice",
		QuestionID: "uc-abc123",
		Question:   "夜间跟车用哪盏灯？",
		Options: []interaction.Option{
			{ID: "o0", Label: "A. 远光灯"},
			{ID: "o1", Label: "B. 近光灯"},
			{ID: "o2", Label: "C. 示廓灯", Description: "示宽"},
		},
		Mode:      "single",
		InputHint: "或输入你的答案",
		Timeout:   600,
		TimeoutAt: 1788339000000,
		Via:       "web",
	}

	// 1. 事件总线存 payload（模拟 PublishToolProgress）
	event := outbound.Event{
		Type: outbound.EventLLMToolProgress,
		Data: map[string]any{
			"toolCallId":   "tc-001",
			"tool":         "user_choice",
			"invocationId": "inv-001",
			"payload":      ev,
		},
	}

	// 2. 翻译为 SSE map
	_, sseMap := translateEventToSSE(event)
	if _, ok := sseMap["payload"]; !ok {
		t.Fatal("SSE map 缺少 payload 字段")
	}

	// 3. SSE 写入端序列化（json）
	raw, err := json.Marshal(sseMap)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SSE data: %s", string(raw))

	// 4. 前端 services.js 解析
	var parts map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatal(err)
	}

	// 5. services.js tool_progress 分支展开 parts.payload
	payload := map[string]any{}
	if p, ok := parts["payload"].(map[string]any); ok {
		for k, v := range p {
			payload[k] = v
		}
	} else {
		t.Fatalf("parts.payload 不是对象（实际 %T）—— 后端序列化把结构体变成了非对象", parts["payload"])
	}
	payload["stream"] = "stdout"
	payload["chunk"] = ""

	// 6. extractChoicePayload 形态三判定
	isChoice := payload["type"] == "user_choice"
	qid, _ := payload["questionId"].(string)
	if !isChoice || qid == "" {
		t.Fatalf("extractChoicePayload 会返回 null！type=%v questionId=%v（前端拿不到卡片）",
			payload["type"], qid)
	}

	// options 必须带 id，否则 ChoiceCard 的 .filter(o=>o.id!=null) 全滤掉 → 无选项
	optsRaw, ok := payload["options"].([]any)
	if !ok {
		t.Fatalf("options 不是数组（实际 %T）—— 前端选项为空", payload["options"])
	}
	if len(optsRaw) != 3 {
		t.Fatalf("期望 3 个选项，实际 %d", len(optsRaw))
	}
	for i, o := range optsRaw {
		m, ok := o.(map[string]any)
		if !ok {
			t.Fatalf("option[%d] 不是对象: %T", i, o)
		}
		if m["id"] == nil {
			t.Errorf("option[%d] 的 id 为 nil —— ChoiceCard 会把它过滤掉，该选项不可点", i)
		}
		t.Logf("  option[%d]: id=%v label=%v", i, m["id"], m["label"])
	}
}
