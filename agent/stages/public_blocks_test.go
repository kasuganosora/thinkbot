package stages

import (
	"context"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"

	"go.uber.org/zap"
)

const (
	incidentFiller = "我先核对一下资料，这个先不猜"
	incidentAnswer = "核对过了：手册第 3 节写的是蓝色主题，不是绿色。需要的话我可以把小节号记下来。"
)

// TestExtractPublicReply_IncidentSecondBlockDelivered：过场话和最终答复是两个
// 不同的 <public> 区块，最终答复不能丢。
func TestExtractPublicReply_IncidentSecondBlockDelivered(t *testing.T) {
	got := sharedClean(histMultiBlockReplies["incident1006"])
	want := incidentFiller + "\n\n" + incidentAnswer
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestExtractPublicReply_MultiBlockRules(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"near-identical (one-char typo) sent once", histMultiBlockReplies["dup0926Typo"],
			"蓝色主题的配置写在设置页第三节，改完要重启服务才会生效，请不要把旧缓存当成当前值。灯灭了再看一次状态页。"},
		{"status + correction both kept", histMultiBlockReplies["distinct0927Correction"],
			"抱歉，刚才只说了没真发出去……这次真的发了！\n\n又没发出去，这次是权限被拦了。请在那边直接说一句「发一条演示帖」，我收到就能发。"},
		{"5fb36fa half/full-width duplicate sent once",
			"<public>喵~6/6全对!💯 这是给谁复习呀🤔</public><public>喵~6/6全对！💯 这是给谁复习呀🤔</public>",
			"喵~6/6全对!💯 这是给谁复习呀🤔"},
		{"short punctuation variant sent once", "<public>好的！</public><public>好的～</public>", "好的！"},
		{"identical emoji-only sent once", "<public>😂</public><public>😂</public>", "😂"},
		{"different emoji-only both kept", "<public>😂</public><public>👍</public>", "😂\n\n👍"},
		{"three blocks: dup in the middle dropped", "<public>先查一下</public><public>先查一下。</public><public>查到了：答案是 42</public>",
			"先查一下\n\n查到了：答案是 42"},
		{"empty and internal-only blocks skipped", "<public> </public><public><internal>只有心里话</internal></public><public>真正的回复</public>", "真正的回复"},
		{"public nested in top-level internal is private", "<internal>想想 <public>SECRET</public> 算了</internal><public>ok</public>", "ok"},
		{"unclosed trailing public not taken", "<public>完整的一句</public><public>被截断的半", "完整的一句"},
		{"trailing bare answer after one public kept", "<public>稍等我查一下</public>查到了：答案是蓝色主题。",
			"稍等我查一下\n\n查到了：答案是蓝色主题。"},
		{"trailing punctuation-only noise dropped", "<public>好的</public>（", "好的"},
		{"trailing whitespace only dropped", "<public>好的</public>\n\n  ", "好的"},
		{"trailing after public with closed internal kept", "<public>先说一句</public>正文答复<internal>心里话</internal>",
			"先说一句\n\n正文答复"},
		{"interstitial bare before later public dropped", "<public>a</public><internal>x</internal> bare <public>b</public>", "a\n\nb"},
		{"all empty blocks", "<public></public><internal>静默观察，不打扰</internal><public></public>\n@@REPLY_CONTROL@@{\"send\": false}", ""},
		// 0925 线上形态：<internal> 被错写的 </public> "闭合"，真正的 </internal> 在后面。
		{"stray close inside internal", "<internal>想法</public><internal>\n更多想法\n</internal><public>@someone 真的是这样（苦笑）</public>\n\n@@REPLY_CONTROL@@{\"send\": true}",
			"@someone 真的是这样（苦笑）"},
	}
	for _, c := range cases {
		if got := sharedClean(c.raw); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// TestExtractPublicReply_RewriteDeduped：0929 Web 回复把同一段话改写了一遍（前缀 0.83 / 全文 0.58），
// 只发第一版。
func TestExtractPublicReply_RewriteDeduped(t *testing.T) {
	got := sharedClean(histMultiBlockReplies["rewrite0929"])
	if !strings.HasPrefix(got, "收到收到，嘴已焊死") || strings.Contains(got, "库你就留着吧") {
		t.Fatalf("rewrite should be deduped to the first version, got %q", got)
	}
}

// TestExtractPublicReply_GarbledTurnKeepsFinalAnswer：前面几个区块是崩坏输出，
// 最后一步才是真正的说明；只取第一块时这段会丢。
func TestExtractPublicReply_GarbledTurnKeepsFinalAnswer(t *testing.T) {
	got := sharedClean(histMultiBlockReplies["garbled0927"])
	if !strings.HasSuffix(got, "扫完说一声，我马上验证真实状态（这次只报验证过的事实，不再抢跑喵）。") {
		t.Fatalf("final step's public block must be delivered, got %q", got)
	}
	if strings.Contains(got, "上一个回复完全崩溃了") {
		t.Fatalf("internal leaked: %q", got)
	}
}

func TestPublicBlockSimilarityThresholds(t *testing.T) {
	var set publicBlockSet
	set.add(incidentFiller)
	if set.isDuplicate(incidentAnswer) {
		t.Fatal("filler and real answer must not be duplicates")
	}
	if !set.isDuplicate("我先核对一下资料,这个先不猜!") {
		t.Fatal("punctuation/width variant must be a duplicate")
	}
	// prefixVerdict 不能与 isDuplicate 矛盾：提前判定 distinct 的必须确实不是重复。
	long := strings.Repeat("完全不同的另一段内容", 6)
	if decided, distinct := set.prefixVerdict(long); !decided || !distinct || set.isDuplicate(long) {
		t.Fatalf("prefixVerdict(long) = %v,%v", decided, distinct)
	}
	var short publicBlockSet
	short.add("第一段公开。")
	if short.isDuplicate("第二段公开。") {
		t.Fatal("short blocks differing in a meaningful char must not be duplicates")
	}
	if !short.isDuplicate("第一段公开！") {
		t.Fatal("short punctuation variant must be a duplicate")
	}
	if decided, _ := set.prefixVerdict("短"); decided {
		t.Fatal("short block must not be decided early")
	}
}

// TestOutputCleanStream_MultiBlockParity：真实多区块样本在任意切法下，流式拼接结果与
// 渠道发送 / 落库内容（extractPublicReply）逐字相同。
func TestOutputCleanStream_MultiBlockParity(t *testing.T) {
	raws := map[string]string{
		"near dup short":           "<public>好的！</public><internal>x</internal><public>好的～</public>",
		"three blocks":             "<public>先查一下</public><public>先查一下。</public><public>查到了：答案是 42</public>",
		"emoji":                    "<public>😂</public>\n<public>👍</public>",
		"long distinct":            "<public>稍等</public><public>" + strings.Repeat("这是第二段真正的回答内容，", 8) + "</public>",
		"long prefix dup":          "<public>" + strings.Repeat("一模一样的开头文字", 8) + "结尾A</public><public>" + strings.Repeat("一模一样的开头文字", 8) + "结尾B</public>",
		"prefix same tail differs": "<public>" + strings.Repeat("同样的开头", 12) + "</public><public>" + strings.Repeat("同样的开头", 12) + strings.Repeat("后面完全不一样的大段补充说明", 10) + "</public>",
		"nested in internal":       "<internal>想想 <public>SECRET</public> 算了</internal><public>ok</public>",
		"unclosed second short":    "<public>完整的一句</public><public>被截断的半",
	}
	for k, v := range histMultiBlockReplies {
		// garbled* 是模型崩坏输出，<public> 之前有大段裸文本（流式已知限制：见
		// TestOutputCleanStream_KnownLimitation），且 strayTagRe 会把 "/data/..." 吞到下一个 '>'，
		// 两者与多区块无关、改动前就不一致；只在下面单独断言最终答复不丢。
		if strings.HasPrefix(k, "garbled") {
			continue
		}
		raws[k] = v
	}
	for name, raw := range raws {
		want := sharedClean(raw)
		for _, chunks := range allChunkings(raw) {
			got, outs := streamClean(chunks, true)
			if got != want {
				t.Fatalf("%s: chunks=%q\n got %q\nwant %q", name, chunks, got, want)
			}
			for _, o := range outs {
				if strings.Contains(o, "SECRET") {
					t.Fatalf("%s: secret leaked in %q", name, o)
				}
			}
		}
	}
}

// TestOutputCleanStream_SecondBlockStreamsLive：确定不是重复后，第二个区块不等闭合就实时推送。
func TestOutputCleanStream_SecondBlockStreamsLive(t *testing.T) {
	f := NewOutputCleanStreamFilter(true)
	if got := f.Feed("<public>" + incidentFiller + "</public>"); got != incidentFiller {
		t.Fatalf("first block: %q", got)
	}
	if got := f.Feed("<public>这个"); got != "" {
		t.Fatalf("second block must be held until it is known not to be a duplicate, got %q", got)
	}
	if got := f.Feed("手册第三节写的是蓝色主题不是绿色"); got != "" {
		t.Fatalf("still too short to decide, got %q", got)
	}
	rest := "需要的话可以把小节号记下来，避免下次再把缓存当成当前值，演示环境每月重置（"
	if got := f.Feed(rest); got != "\n\n这个手册第三节写的是蓝色主题不是绿色"+rest {
		t.Fatalf("distinct block should be released once decided, got %q", got)
	}
	if got := f.Feed("演示数据每月重置。"); got != "演示数据每月重置。" {
		t.Fatalf("then streams live, got %q", got)
	}
	if got := f.Feed("</public>\n"); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := f.Flush(); got != "" {
		t.Fatalf("flush got %q", got)
	}
}

// incidentChunks 把「过场话 → 最终答复」按增量切开，result.Text 跨步骤拼接。
func incidentChunks() []string {
	return []string{
		"<public>我先核对一下资料，", "这个先不猜</public>",
		"<public>核对过了：手册第 3 节写的是蓝色主题，不是绿色。",
		"需要的话我可以把小节号记下来。</public>",
		"\n\n@@REPLY_CONTROL@@{\"send\": true}",
	}
}

// TestProcess_MultiBlockReplyPayload 端到端：Telegram 私聊（渠道发送 + chat_messages 落库都用
// ActionReply payload）与 Web 私聊（流式 + payload）对多区块回复给出同一内容。
func TestProcess_MultiBlockReplyPayload(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"incident: filler + real answer", incidentChunks(), incidentFiller + "\n\n" + incidentAnswer},
		{"near-identical blocks sent once",
			[]string{"<public>喵~6/6全对!💯 这是给谁复习呀🤔</public>", "<public>喵~6/6全对！💯 这是给谁复习呀🤔</public>", "\n@@REPLY_CONTROL@@{\"send\": true}"},
			"喵~6/6全对!💯 这是给谁复习呀🤔"},
		{"missing control: explicit public fallback keeps both blocks",
			[]string{"<public>先查一下</public>", "<internal>查到了</internal><public>答案是 42</public>"},
			"先查一下\n\n答案是 42"},
		{"trailing bare after one public (2026-10-06 22:45)",
			[]string{"<public>" + incidentTrailingStub + "</public>", incidentTrailingAnswer, "\n\n@@REPLY_CONTROL@@{\"send\": true}"},
			incidentTrailingStub + "\n\n" + incidentTrailingAnswer},
	}
	for _, c := range cases {
		for _, source := range []string{"telegram", "web"} {
			t.Run(c.name+"/"+source, func(t *testing.T) {
				pub := &recordingPublisher{}
				cfg := LLMConfig{
					RequireReplyControl: true,
					MessageBuilder: func(msg core.Message) []llm.Message {
						return []llm.Message{llm.UserMessage(msg.Text)}
					},
				}
				if source == "web" {
					cfg.StreamPublisher = pub
				}
				stage := NewLLMStage("llm", &chunkStreamProvider{chunks: c.chunks}, cfg, nil, nil)
				env := core.NewEnvelope(core.Message{
					ID: "m-1", TraceID: "t-1", BotID: "b1", Text: "蓝色主题写在哪一节", Source: source,
					Channel: source, UserID: "demo-user", ChatType: core.ChatPrivate,
				})
				out, err := stage.Process(context.Background(), env)
				if err != nil {
					t.Fatalf("Process: %v", err)
				}
				replies := replyActions(out)
				if len(replies) != 1 {
					t.Fatalf("want 1 reply action, got %d", len(replies))
				}
				payload, _ := replies[0].Payload.(string)
				if payload != c.want {
					t.Fatalf("payload = %q, want %q", payload, c.want)
				}
				if source == "web" {
					if streamed := strings.Join(pub.deltas, ""); streamed != payload {
						t.Fatalf("streamed %q != payload %q", streamed, payload)
					}
				}
			})
		}
	}
}

// incidentTrailingPublic：只有一个 <public> 过场句，真正答复写在 </public> 之后的裸文本里。
const (
	incidentTrailingStub   = "等等，我先把手册翻开（"
	incidentTrailingAnswer = "翻完了。手册第 3 节写的是蓝色主题，不是绿色。"
)

func TestExtractPublicReply_TrailingBareAnswerAfterPublic(t *testing.T) {
	raw := "<public>" + incidentTrailingStub + "</public>" + incidentTrailingAnswer +
		"\n\n@@REPLY_CONTROL@@{\"send\": true}"
	got := sharedClean(raw)
	want := incidentTrailingStub + "\n\n" + incidentTrailingAnswer
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	// 缺失控制块的 fallback（explicitPublicReply）也必须带上 trailing。
	if got2 := explicitPublicReply(CleanOutboundText(raw)); got2 != want {
		t.Fatalf("explicitPublicReply: got %q\nwant %q", got2, want)
	}
}

func TestAppendStepText_InsertsNewlineBetweenSteps(t *testing.T) {
	cases := []struct {
		dst, next, want string
	}{
		{"等栞娜查查", "查完啦", "等栞娜查查\n查完啦"},
		{"等栞娜查查\n", "查完啦", "等栞娜查查\n查完啦"},
		{"等栞娜查查", "\n查完啦", "等栞娜查查\n查完啦"},
		{"", "查完啦", "查完啦"},
		{"等栞娜查查", "", "等栞娜查查"},
	}
	for _, c := range cases {
		if got := appendStepText(c.dst, c.next); got != c.want {
			t.Errorf("appendStepText(%q, %q) = %q, want %q", c.dst, c.next, got, c.want)
		}
	}
}

// stepSepStreamProvider 模拟「文本 → 工具 → 文本」两步流，用于验证跨步拼接插入换行。
type stepSepStreamProvider struct{}

func (stepSepStreamProvider) Name() string { return "step-sep" }
func (stepSepStreamProvider) DoGenerate(context.Context, llm.GenerateParams) (*llm.GenerateResult, error) {
	return &llm.GenerateResult{Text: "等栞娜查查\n查完啦"}, nil
}
func (stepSepStreamProvider) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	ch := make(chan llm.StreamPart, 16)
	go func() {
		defer close(ch)
		ch <- &llm.TextDeltaPart{Text: "等栞娜查查"}
		ch <- &llm.StreamToolCallPart{ToolCallID: "c1", ToolName: "web_fetch", Input: map[string]any{"url": "x"}}
		ch <- &llm.FinishStepPart{FinishReason: llm.FinishReasonToolCalls, Usage: llm.Usage{TotalTokens: 1}}
		ch <- &llm.StreamToolResultPart{ToolCallID: "c1", ToolName: "web_fetch", Output: "ok"}
		ch <- &llm.TextDeltaPart{Text: "查完啦"}
		ch <- &llm.FinishStepPart{FinishReason: llm.FinishReasonStop, Usage: llm.Usage{TotalTokens: 2}}
		ch <- &llm.FinishPart{FinishReason: llm.FinishReasonStop, TotalUsage: llm.Usage{TotalTokens: 3}}
	}()
	return &llm.StreamResult{Stream: ch}, nil
}

func TestProcessStream_StepTextConcatInsertsNewline(t *testing.T) {
	pub := &recordingPublisher{}
	s := &LLMStage{
		name:     "llm",
		provider: stepSepStreamProvider{},
		config:   LLMConfig{StreamPublisher: pub},
		logger:   zap.NewNop().Sugar(),
	}
	env := &core.Envelope{Message: core.Message{ID: "m1", TraceID: "t1", BotID: "b1"}}
	res, err := s.processStream(context.Background(), env, &llm.OrchestrateConfig{MaxSteps: 0}, zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("processStream: %v", err)
	}
	// MaxSteps=0 → OrchestrateStream 直接走 provider DoStream，我们的 channel 原样转发。
	if res.Text != "等栞娜查查\n查完啦" {
		t.Fatalf("result.Text = %q, want newline between steps", res.Text)
	}
	if strings.Contains(res.Text, "查查查完") {
		t.Fatalf("glued without separator: %q", res.Text)
	}
}

// TestCleanPublicBlock_StripsRecalledMemory 覆盖跨会话召回的出站硬闸：
// 模型把 system prompt 里的记忆块整段复述进回复时，必须连内容一起剥掉——
// 私聊记忆被带到群里后，只去标签（strayTagRe）会把正文原样留在回复里发出去。
func TestCleanPublicBlock_StripsRecalledMemory(t *testing.T) {
	in := "先说结论\n<recalled_memory>\nMEMORY (your personal notes)\n测试数据库 192.0.2.10:3306 用户 appuser\n</recalled_memory>\n再说细节"
	got := cleanPublicBlock(in)
	if strings.Contains(got, "192.0.2.10") || strings.Contains(got, "appuser") {
		t.Fatalf("recalled memory leaked into outbound text: %q", got)
	}
	if strings.Contains(got, "recalled_memory") {
		t.Fatalf("recall tag leaked: %q", got)
	}
	if !strings.Contains(got, "先说结论") || !strings.Contains(got, "再说细节") {
		t.Fatalf("normal reply content must survive: %q", got)
	}
}

// TestOutputCleanStreamFilter_DropsRecalledMemory 覆盖流式侧：Web 流是实时可见的，
// 「门控事后删」等于已经泄露，所以 <recalled_memory> 必须像 <internal> 一样在流里就被吞掉。
func TestOutputCleanStreamFilter_DropsRecalledMemory(t *testing.T) {
	f := NewOutputCleanStreamFilter(true)
	// 分块喂入，模拟真实流式：标签与内容跨 chunk 到达
	chunks := []string{
		"<public>可见开头 ",
		"<recalled_memory>",
		"私聊里说的秘密 192.0.2.10",
		"</recalled_memory>",
		"可见结尾</public>",
	}
	var got string
	for _, c := range chunks {
		got += f.Feed(c)
	}
	got += f.Flush()
	if strings.Contains(got, "192.0.2.10") || strings.Contains(got, "recalled_memory") {
		t.Fatalf("stream leaked recalled memory: %q", got)
	}
	if !strings.Contains(got, "可见开头") || !strings.Contains(got, "可见结尾") {
		t.Fatalf("normal content lost: %q", got)
	}
}

// TestAppendReplyControlReminder_Idempotent 覆盖逐步提醒的幂等性。
//
// 这个不变量一旦破了，第 N 步就会堆 N 条提醒：applyPrepareStep 把返回的
// messages 赋回持久历史，30 步的复杂任务会白白吃掉上万字符上下文，
// 反而把协议本身稀释掉（正是要修的问题）。
func TestAppendReplyControlReminder_Idempotent(t *testing.T) {
	msgs := []llm.Message{
		llm.SystemMessage("base system"),
		llm.UserMessage("查一下这个 bug"),
		llm.AssistantMessage("先看日志"),
	}

	once := appendReplyControlReminder(msgs)
	if len(once) != len(msgs)+1 {
		t.Fatalf("want %d messages, got %d", len(msgs)+1, len(once))
	}
	last := once[len(once)-1]
	if last.Role != llm.MessageRoleSystem {
		t.Errorf("reminder must be a system message, got %q", last.Role)
	}
	if llm.TextFromParts(last.Content) != replyControlReminder {
		t.Errorf("reminder must sit at the very end, got %q", llm.TextFromParts(last.Content))
	}

	// 模拟 30 步连续调用：条数不能增长
	cur := msgs
	for i := 0; i < 30; i++ {
		cur = appendReplyControlReminder(cur)
		// 每步之间模型/工具还会产生新消息
		cur = append(cur, llm.ToolMessage(llm.ToolResultPart{
			ToolCallID: "c1", ToolName: "exec", Result: "ok",
		}))
	}
	n := 0
	for _, m := range cur {
		if m.Role == llm.MessageRoleSystem && llm.TextFromParts(m.Content) == replyControlReminder {
			n++
		}
	}
	if n != 1 {
		t.Errorf("after 30 steps there must be exactly one reminder, got %d", n)
	}
	if got := llm.TextFromParts(cur[len(cur)-1].Content); got == replyControlReminder {
		t.Error("reminder should be re-appended after new messages, but must not duplicate")
	}
	// 重新顶到末尾后仍是唯一一条
	final := appendReplyControlReminder(cur)
	cnt := 0
	idx := -1
	for i, m := range final {
		if m.Role == llm.MessageRoleSystem && llm.TextFromParts(m.Content) == replyControlReminder {
			cnt++
			idx = i
		}
	}
	if cnt != 1 || idx != len(final)-1 {
		t.Errorf("want exactly one reminder at the end, got count=%d idx=%d len=%d", cnt, idx, len(final))
	}
}
